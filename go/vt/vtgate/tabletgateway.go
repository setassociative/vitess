/*
Copyright 2019 The Vitess Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package vtgate

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"runtime/debug"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/spf13/pflag"

	"vitess.io/vitess/go/mysql/collations"
	"vitess.io/vitess/go/vt/discovery"
	"vitess.io/vitess/go/vt/log"
	"vitess.io/vitess/go/vt/logutil"
	"vitess.io/vitess/go/vt/servenv"
	"vitess.io/vitess/go/vt/srvtopo"
	"vitess.io/vitess/go/vt/topo"
	"vitess.io/vitess/go/vt/topo/topoproto"
	"vitess.io/vitess/go/vt/vterrors"
	"vitess.io/vitess/go/vt/vtgate/balancer"
	"vitess.io/vitess/go/vt/vtgate/buffer"
	"vitess.io/vitess/go/vt/vttablet/queryservice"

	querypb "vitess.io/vitess/go/vt/proto/query"
	topodatapb "vitess.io/vitess/go/vt/proto/topodata"
	vtrpcpb "vitess.io/vitess/go/vt/proto/vtrpc"
)

var (
	_ discovery.HealthCheck = (*discovery.HealthCheckImpl)(nil)
	// CellsToWatch is the list of cells the healthcheck operates over. If it is empty, only the local cell is watched
	CellsToWatch string

	initialTabletTimeout = 30 * time.Second
	// retryCount is the number of times a query will be retried on error
	retryCount = 2

	// configuration flags for the tablet balancer
	balancerEnabled     bool
	balancerVtgateCells []string
	balancerKeyspaces   []string

	logCollations = logutil.NewThrottledLogger("CollationInconsistent", 1*time.Minute)
)

func init() {
	servenv.OnParseFor("vtgate", func(fs *pflag.FlagSet) {
		fs.StringVar(&CellsToWatch, "cells_to_watch", "", "comma-separated list of cells for watching tablets")
		fs.DurationVar(&initialTabletTimeout, "gateway_initial_tablet_timeout", 30*time.Second, "At startup, the tabletGateway will wait up to this duration to get at least one tablet per keyspace/shard/tablet type")
		fs.IntVar(&retryCount, "retry-count", 2, "retry count")
		fs.BoolVar(&balancerEnabled, "enable-balancer", false, "Enable the tablet balancer to evenly spread query load for a given tablet type")
		fs.StringSliceVar(&balancerVtgateCells, "balancer-vtgate-cells", []string{}, "When in balanced mode, a comma-separated list of cells that contain vtgates (required)")
		fs.StringSliceVar(&balancerKeyspaces, "balancer-keyspaces", []string{}, "When in balanced mode, a comma-separated list of keyspaces for which to use the balancer (optional)")
	})
}

// TabletGateway implements the Gateway interface.
// This implementation uses the new healthcheck module.
type TabletGateway struct {
	queryservice.QueryService
	hc                   discovery.HealthCheck
	kev                  *discovery.KeyspaceEventWatcher
	srvTopoServer        srvtopo.Server
	localCell            string
	retryCount           int
	defaultConnCollation atomic.Uint32

	// mu protects the fields of this group.
	mu sync.Mutex
	// statusAggregators is a map indexed by the key
	// keyspace/shard/tablet_type.
	statusAggregators map[string]*TabletStatusAggregator

	// buffer, if enabled, buffers requests during a detected PRIMARY failover.
	buffer *buffer.Buffer

	// balancer used for routing to tablets
	balancer balancer.TabletBalancer
}

func createHealthCheck(ctx context.Context, retryDelay, timeout time.Duration, ts *topo.Server, cell, cellsToWatch string) discovery.HealthCheck {
	filters, err := discovery.NewVTGateHealthCheckFilters()
	if err != nil {
		log.Exit(err)
	}
	return discovery.NewHealthCheck(ctx, retryDelay, timeout, ts, cell, cellsToWatch, filters)
}

// NewTabletGateway creates and returns a new TabletGateway
func NewTabletGateway(ctx context.Context, hc discovery.HealthCheck, serv srvtopo.Server, localCell string) *TabletGateway {
	// hack to accommodate various users of gateway + tests
	if hc == nil {
		var topoServer *topo.Server
		if serv != nil {
			var err error
			topoServer, err = serv.GetTopoServer()
			if err != nil {
				log.Exitf("Unable to create new TabletGateway: %v", err)
			}
		}
		hc = createHealthCheck(ctx, healthCheckRetryDelay, healthCheckTimeout, topoServer, localCell, CellsToWatch)
	}
	gw := &TabletGateway{
		hc:                hc,
		srvTopoServer:     serv,
		localCell:         localCell,
		retryCount:        retryCount,
		statusAggregators: make(map[string]*TabletStatusAggregator),
	}
	gw.setupBuffering(ctx)
	if balancerEnabled {
		gw.setupBalancer(ctx)
	}
	gw.QueryService = queryservice.Wrap(nil, gw.withRetry)
	return gw
}

func (gw *TabletGateway) setupBuffering(ctx context.Context) {
	cfg := buffer.NewConfigFromFlags()
	if !cfg.Enabled {
		log.Info("Query buffering is disabled")
		return
	}
	gw.buffer = buffer.New(cfg)

	gw.kev = discovery.NewKeyspaceEventWatcher(ctx, gw.srvTopoServer, gw.hc, gw.localCell)
	ksChan := gw.kev.Subscribe()
	bufferCtx, bufferCancel := context.WithCancel(ctx)

	go func(ctx context.Context, c chan *discovery.KeyspaceEvent, buffer *buffer.Buffer) {
		defer bufferCancel()

		for {
			select {
			case <-ctx.Done():
				return
			case result := <-ksChan:
				if result == nil {
					return
				}
				buffer.HandleKeyspaceEvent(result)
			}
		}
	}(bufferCtx, ksChan, gw.buffer)
}

func (gw *TabletGateway) setupBalancer(ctx context.Context) {
	if len(balancerVtgateCells) == 0 {
		log.Exitf("balancer-vtgate-cells is required for balanced mode")
	}
	gw.balancer = balancer.NewTabletBalancer(gw.localCell, balancerVtgateCells)
}

// QueryServiceByAlias satisfies the Gateway interface
func (gw *TabletGateway) QueryServiceByAlias(ctx context.Context, alias *topodatapb.TabletAlias, target *querypb.Target) (queryservice.QueryService, error) {
	qs, err := gw.hc.TabletConnection(ctx, alias, target)
	return queryservice.Wrap(qs, gw.withShardError), NewShardError(err, target)
}

// GetServingKeyspaces returns list of serving keyspaces.
func (gw *TabletGateway) GetServingKeyspaces() []string {
	if gw.kev == nil {
		return nil
	}
	return gw.kev.GetServingKeyspaces()
}

// RegisterStats registers the stats to export the lag since the last refresh
// and the checksum of the topology
func (gw *TabletGateway) RegisterStats() {
	gw.hc.RegisterStats()
}

// WaitForTablets is part of the Gateway interface.
func (gw *TabletGateway) WaitForTablets(ctx context.Context, tabletTypesToWait []topodatapb.TabletType) (err error) {
	log.Infof("Gateway waiting for serving tablets of types %v ...", tabletTypesToWait)
	ctx, cancel := context.WithTimeout(ctx, initialTabletTimeout)
	defer cancel()

	defer func() {
		switch err {
		case nil:
			// Log so we know everything is fine.
			log.Infof("Waiting for tablets completed")
		case context.DeadlineExceeded:
			// In this scenario, we were able to reach the
			// topology service, but some tablets may not be
			// ready. We just warn and keep going.
			log.Warningf("Timeout waiting for all keyspaces / shards to have healthy tablets of types %v, may be in degraded mode", tabletTypesToWait)
			err = nil
		}
	}()

	// Skip waiting for tablets if we are not told to do so.
	if len(tabletTypesToWait) == 0 {
		return nil
	}

	// Finds the targets to look for.
	targets, keyspaces, err := srvtopo.FindAllTargetsAndKeyspaces(ctx, gw.srvTopoServer, gw.localCell, discovery.KeyspacesToWatch, tabletTypesToWait)
	if err != nil {
		return err
	}
	err = gw.hc.WaitForAllServingTablets(ctx, targets)
	if err != nil {
		return err
	}
	// After having waited for all serving tablets. We should also wait for the keyspace event watcher to have seen
	// the updates and marked all the keyspaces as consistent (if we want to wait for primary tablets).
	// Otherwise, we could be in a situation where even though the healthchecks have arrived, the keyspace event watcher hasn't finished processing them.
	// So, if a primary tablet goes non-serving (because of a PRS or some other reason), we won't be able to start buffering.
	// Waiting for the keyspaces to become consistent ensures that all the primary tablets for all the shards should be serving as seen by the keyspace event watcher
	// and any disruption from now on, will make sure we start buffering properly.
	if topoproto.IsTypeInList(topodatapb.TabletType_PRIMARY, tabletTypesToWait) && gw.kev != nil {
		return gw.kev.WaitForConsistentKeyspaces(ctx, keyspaces)
	}
	return nil
}

// Close shuts down underlying connections.
// This function hides the inner implementation.
func (gw *TabletGateway) Close(_ context.Context) error {
	if gw.buffer != nil {
		gw.buffer.Shutdown()
	}
	return gw.hc.Close()
}

// CacheStatus returns a list of TabletCacheStatus per
// keyspace/shard/tablet_type.
func (gw *TabletGateway) CacheStatus() TabletCacheStatusList {
	gw.mu.Lock()
	res := make(TabletCacheStatusList, 0, len(gw.statusAggregators))
	for _, aggr := range gw.statusAggregators {
		res = append(res, aggr.GetCacheStatus())
	}
	gw.mu.Unlock()
	sort.Sort(res)
	return res
}

func (gw *TabletGateway) DebugBalancerHandler(w http.ResponseWriter, r *http.Request) {
	if balancerEnabled {
		gw.balancer.DebugHandler(w, r)
	} else {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("not enabled"))
	}
}

// withRetry gets available connections and executes the action. If there are retryable errors,
// it retries retryCount times before failing. It does not retry if the connection is in
// the middle of a transaction. While returning the error check if it maybe a result of
// a resharding event, and set the re-resolve bit and let the upper layers
// re-resolve and retry.
//
// withRetry also adds shard information to errors returned from the inner QueryService, so
// withShardError should not be combined with withRetry.
func (gw *TabletGateway) withRetry(ctx context.Context, target *querypb.Target, _ queryservice.QueryService,
	_ string, inTransaction bool, inner func(ctx context.Context, target *querypb.Target, conn queryservice.QueryService) (bool, error)) error {

	// for transactions, we connect to a specific tablet instead of letting gateway choose one
	if inTransaction && target.TabletType != topodatapb.TabletType_PRIMARY {
		return vterrors.Errorf(vtrpcpb.Code_INTERNAL, "tabletGateway's query service can only be used for non-transactional queries on replicas")
	}
	var tabletLastUsed *topodatapb.Tablet
	var err error
	invalidTablets := make(map[string]bool)

	if len(discovery.AllowedTabletTypes) > 0 {
		var match bool
		for _, allowed := range discovery.AllowedTabletTypes {
			if allowed == target.TabletType {
				match = true
				break
			}
		}
		if !match {
			return vterrors.Errorf(vtrpcpb.Code_FAILED_PRECONDITION, "requested tablet type %v is not part of the allowed tablet types for this vtgate: %+v", target.TabletType.String(), discovery.AllowedTabletTypes)
		}
	}

	bufferedOnce := false
	for i := 0; i < gw.retryCount+1; i++ {
		// Check if we should buffer PRIMARY queries which failed due to an ongoing failover.
		// Note: We only buffer once and only "!inTransaction" queries i.e.
		// a) no transaction is necessary (e.g. critical reads) or
		// b) no transaction was created yet.
		if gw.buffer != nil && !bufferedOnce && !inTransaction && target.TabletType == topodatapb.TabletType_PRIMARY {
			// The next call blocks if we should buffer during a failover.
			retryDone, bufferErr := gw.buffer.WaitForFailoverEnd(ctx, target.Keyspace, target.Shard, gw.kev, err)

			// Request may have been buffered.
			if retryDone != nil {
				// We're going to retry this request as part of a buffer drain.
				// Notify the buffer after we retried.
				defer retryDone()
				bufferedOnce = true
			}

			if bufferErr != nil {
				err = vterrors.Wrapf(bufferErr,
					"failed to automatically buffer and retry failed request during failover. original err (type=%T): %v",
					err, err)
				break
			}
		}

		tablets := gw.hc.GetHealthyTabletStats(target)
		if len(tablets) == 0 {
			// if we have a keyspace event watcher, check if the reason why our primary is not available is that it's currently being resharded
			// or if a reparent operation is in progress.
			// We only check for whether reshard is ongoing or primary is serving or not, only if the target is primary. We don't want to buffer
			// replica queries, so it doesn't make any sense to check for resharding or reparenting in that case.
			if kev := gw.kev; kev != nil && target.TabletType == topodatapb.TabletType_PRIMARY {
				if kev.TargetIsBeingResharded(ctx, target) {
					log.V(2).Infof("current keyspace is being resharded, retrying: %s: %s", target.Keyspace, debug.Stack())
					err = vterrors.Errorf(vtrpcpb.Code_CLUSTER_EVENT, buffer.ClusterEventReshardingInProgress)
					continue
				}
				primary, shouldBuffer := kev.ShouldStartBufferingForTarget(ctx, target)
				if shouldBuffer {
					err = vterrors.Errorf(vtrpcpb.Code_CLUSTER_EVENT, buffer.ClusterEventReparentInProgress)
					continue
				}
				// if the keyspace event manager doesn't think we should buffer queries, and also sees a primary tablet,
				// but we initially found no tablet, we're in an inconsistent state
				// we then retry the entire loop
				if primary != nil {
					err = vterrors.Errorf(vtrpcpb.Code_UNAVAILABLE, "inconsistent state detected, primary is serving but initially found no available tablet")
					continue
				}
			}

			// fail fast if there is no tablet
			err = vterrors.Errorf(vtrpcpb.Code_UNAVAILABLE, "no healthy tablet available for '%s'", target.String())
			break
		}

		var th *discovery.TabletHealth

		useBalancer := balancerEnabled
		if balancerEnabled && len(balancerKeyspaces) > 0 {
			useBalancer = slices.Contains(balancerKeyspaces, target.Keyspace)
		}
		if useBalancer {
			// filter out the tablets that we've tried before (if any), then pick the best one
			if len(invalidTablets) > 0 {
				tablets = slices.DeleteFunc(tablets, func(t *discovery.TabletHealth) bool {
					_, isInvalid := invalidTablets[topoproto.TabletAliasString(t.Tablet.Alias)]
					return isInvalid
				})
			}

			th = gw.balancer.Pick(target, tablets)

		} else {
			gw.shuffleTablets(gw.localCell, tablets)

			// skip tablets we tried before
			for _, t := range tablets {
				if _, ok := invalidTablets[topoproto.TabletAliasString(t.Tablet.Alias)]; !ok {
					th = t
					break
				}
			}
		}

		if th == nil {
			// do not override error from last attempt.
			if err == nil {
				err = vterrors.VT14002()
			}
			break
		}

		tabletLastUsed = th.Tablet
		// execute
		if th.Conn == nil {
			err = vterrors.VT14003(tabletLastUsed)
			invalidTablets[topoproto.TabletAliasString(tabletLastUsed.Alias)] = true
			continue
		}

		gw.updateDefaultConnCollation(tabletLastUsed)

		startTime := time.Now()
		var canRetry bool
		canRetry, err = inner(ctx, target, th.Conn)
		gw.updateStats(target, startTime, err)
		if canRetry {
			invalidTablets[topoproto.TabletAliasString(tabletLastUsed.Alias)] = true
			continue
		}
		break
	}
	return NewShardError(err, target)
}

// withShardError adds shard information to errors returned from the inner QueryService.
func (gw *TabletGateway) withShardError(ctx context.Context, target *querypb.Target, conn queryservice.QueryService,
	_ string, _ bool, inner func(ctx context.Context, target *querypb.Target, conn queryservice.QueryService) (bool, error)) error {
	_, err := inner(ctx, target, conn)
	return NewShardError(err, target)
}

func (gw *TabletGateway) updateStats(target *querypb.Target, startTime time.Time, err error) {
	elapsed := time.Since(startTime)
	aggr := gw.getStatsAggregator(target)
	aggr.UpdateQueryInfo("", target.TabletType, elapsed, err != nil)
}

func (gw *TabletGateway) getStatsAggregator(target *querypb.Target) *TabletStatusAggregator {
	key := fmt.Sprintf("%v/%v/%v", target.Keyspace, target.Shard, target.TabletType.String())

	// get existing aggregator
	gw.mu.Lock()
	defer gw.mu.Unlock()
	aggr, ok := gw.statusAggregators[key]
	if ok {
		return aggr
	}
	// create a new one if it doesn't exist yet
	aggr = NewTabletStatusAggregator(target.Keyspace, target.Shard, target.TabletType, key)
	gw.statusAggregators[key] = aggr
	return aggr
}

func (gw *TabletGateway) shuffleTablets(cell string, tablets []*discovery.TabletHealth) {

	// Randomly shuffle the list of tablets, putting the same-cell hosts at the front
	// of the list and the other-cell hosts at the back
	//
	// Only need to do n-1 swaps since the last tablet is always in the right place.
	n := len(tablets)
	head := 0
	tail := n - 1
	for i := 0; i < n-1; i++ {
		j := head + rand.IntN(tail-head+1)

		if tablets[j].Tablet.Alias.Cell == cell {
			tablets[head], tablets[j] = tablets[j], tablets[head]
			head++
		} else {
			tablets[tail], tablets[j] = tablets[j], tablets[tail]
			tail--
		}
	}
}

// TabletsCacheStatus returns a displayable version of the health check cache.
func (gw *TabletGateway) TabletsCacheStatus() discovery.TabletsCacheStatusList {
	return gw.hc.CacheStatus()
}

// TabletsHealthyStatus returns a displayable version of the health check healthy list.
func (gw *TabletGateway) TabletsHealthyStatus() discovery.TabletsCacheStatusList {
	return gw.hc.HealthyStatus()
}

func (gw *TabletGateway) updateDefaultConnCollation(tablet *topodatapb.Tablet) {
	if gw.defaultConnCollation.CompareAndSwap(0, tablet.DefaultConnCollation) {
		return
	}
	if gw.defaultConnCollation.Load() != tablet.DefaultConnCollation {
		logCollations.Warningf("this Vitess cluster has tablets with different default connection collations")
	}
}

// DefaultConnCollation returns the default connection collation of this TabletGateway
func (gw *TabletGateway) DefaultConnCollation() collations.ID {
	return collations.ID(gw.defaultConnCollation.Load())
}

// NewShardError returns a new error with the shard info amended.
func NewShardError(in error, target *querypb.Target) error {
	if in == nil {
		return nil
	}
	if target != nil {
		return vterrors.Wrapf(in, "target: %s.%s.%s", target.Keyspace, target.Shard, topoproto.TabletTypeLString(target.TabletType))
	}
	return in
}
