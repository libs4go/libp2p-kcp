package kcp

import (
	"context"
	"fmt"
	"net"
	"testing"

	"github.com/libp2p/go-libp2p"
	"github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/host"
	peerstore "github.com/libp2p/go-libp2p-peerstore"
	grpc "github.com/libs4go/libp2p-grpc"
	"github.com/libs4go/libp2p-kcp/pro"
	"github.com/libs4go/scf4go"
	_ "github.com/libs4go/scf4go/codec" //
	"github.com/libs4go/scf4go/reader/file"
	"github.com/libs4go/slf4go"
	_ "github.com/libs4go/slf4go/backend/console" //
	"github.com/stretchr/testify/require"
)

//go:generate protoc --proto_path=./pro --go_out=plugins=grpc,paths=source_relative:./pro echo.proto

func init() {
	config := scf4go.New()

	err := config.Load(file.New(file.Yaml("./kcp_test.yaml")))

	if err != nil {
		panic(err)
	}

	err = slf4go.Config(config)

	if err != nil {
		panic(err)
	}
}

func makeHost(port int) (host.Host, error) {
	prikey, _, err := crypto.GenerateKeyPair(crypto.Ed25519, 2048)

	if err != nil {
		return nil, err
	}

	kcp, err := New(prikey, WithTLS())

	if err != nil {
		return nil, err
	}

	opts := []libp2p.Option{
		libp2p.ListenAddrStrings(fmt.Sprintf("/ip4/127.0.0.1/udp/%d/kcp", port)),
		libp2p.Identity(prikey),
		libp2p.DisableRelay(),
		libp2p.ChainOptions(
			libp2p.Transport(kcp),
		),
	}

	return libp2p.New(context.Background(), opts...)
}

type echoServer struct {
}

func (s *echoServer) Say(ctx context.Context, request *pro.Request) (*pro.Response, error) {
	return &pro.Response{Message: request.Message}, nil
}

func TestEcho(t *testing.T) {
	h1, err := makeHost(1812)

	require.NoError(t, err)

	h2, err := makeHost(1813)

	require.NoError(t, err)

	h2.Peerstore().AddAddr(h1.ID(), h1.Addrs()[0], peerstore.PermanentAddrTTL)

	t1 := grpc.New(context.Background(), h1)

	t2 := grpc.New(context.Background(), h2)

	s1 := grpc.Serve(t1)

	pro.RegisterEchoServer(s1, &echoServer{})

	conn, err := grpc.Dial(t2, h1.ID())

	require.NoError(t, err)

	client := pro.NewEchoClient(conn)

	resp, err := client.Say(context.Background(), &pro.Request{Message: "hello1"})

	require.NoError(t, err)

	require.Equal(t, "hello1", resp.Message)
}

func TestMultAddr(t *testing.T) {
	addr := &net.UDPAddr{IP: net.IPv4(192, 168, 0, 42), Port: 1337}
	maddr, err := toKcpMultiaddr(addr)
	require.NoError(t, err)

	require.Equal(t, "/ip4/192.168.0.42/udp/1337/kcp", maddr.String())
}
