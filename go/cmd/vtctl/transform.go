package vtctl

import "github.com/golang/protobuf/proto"

// ProtoEncoder will convert a protobuf object into an equivalent value
// using some other format. That result will be returned as a []byte.
//
// If the proto object can not be transformed for any reason an error is
// returned.
type ProtoEncoder interface {
	Encode(proto.Message) ([]byte, error)
}

type ProtoDecoder interface {
	Decode(string, []byte) (proto.Message, error)
}

type ProtoEncoderDecoder interface {
	ProtoEncoder
	ProtoDecoder
}

type msgFactory func()proto.Message

var msgObjFactory map[string]msgFactory

type pair struct {
	name string
	mk msgFactory
}

func init() {
		msgObjFactory = []pair{
			{strings.ToLower(topo.CellInfoFile), func() msgFactory { return new(topodatapb.CellInfo)} },
			{strings.ToLower(topo.KeyspaceFile), func() msgFactory { return new(topodatapb.Keyspace)} },
			{strings.ToLower(topo.ShardFile), func() msgFactory { return new(topodatapb.Shard)} },
			{strings.ToLower(topo.VSchemaFile), func() msgFactory { return new(vschemapb.Keyspace)} },
			{strings.ToLower(topo.ShardReplicationFile), func() msgFactory { return new(topodatapb.ShardReplication)} },
			{strings.ToLower(topo.TabletFile), func() msgFactory { return new(topodatapb.Tablet)} },
			{strings.ToLower(topo.SrvVSchemaFile), func() msgFactory { return new(vschemapb.SrvVSchema)} },
			{strings.ToLower(topo.SrvKeyspaceFile), func() msgFactory { return new(topodatapb.SrvKeyspace)} },
			{strings.ToLower(topo.RoutingRulesFile), func() msgFactory { return new(vschemapb.RoutingRules)} },
		}
}

type byteProtoEncoder struct{}
var _ ProtoEncoder = byteProtoEncoder{}

func (_ byteProtoEncoder) Encode(obj proto.Message) ([]byte, error) {

	return nil, nil
}


type byteProtoDecoder struct{}
var _ ProtoDecoder = byteProtoDecoder{}

func (_ byteProtoDecoder) Decode(typ string, data []byte) (proto.Message, error) {
	return nil, nil
}