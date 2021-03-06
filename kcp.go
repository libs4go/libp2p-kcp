package kcp

import (
	"context"
	"crypto/tls"
	"net"
	"time"

	"github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/mux"
	"github.com/libp2p/go-libp2p-core/peer"
	"github.com/libp2p/go-libp2p-core/transport"
	tlsp2p "github.com/libp2p/go-libp2p-tls"
	"github.com/libs4go/errors"
	"github.com/libs4go/slf4go"
	"github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr-net"
	kcpgo "github.com/xtaci/kcp-go"
	"github.com/xtaci/smux"
)

// ScopeOfAPIError .
const errVendor = "kcp"

// errors
var (
	ErrInternal = errors.New("the internal error", errors.WithVendor(errVendor), errors.WithCode(-1))
	ErrAddr     = errors.New("invalid libp2p net.addr", errors.WithVendor(errVendor), errors.WithCode(-2))
	ErrClosed   = errors.New("transport closed", errors.WithVendor(errVendor), errors.WithCode(-3))
	ErrTLS      = errors.New("expected remote pub key to be set", errors.WithVendor(errVendor), errors.WithCode(-4))
)

const protocolKCPID = 482

var protoKCP = multiaddr.Protocol{
	Name:  "kcp",
	Code:  protocolKCPID,
	VCode: multiaddr.CodeToVarint(protocolKCPID),
}

func init() {
	if err := multiaddr.AddProtocol(protoKCP); err != nil {
		panic(err)
	}
}

// Option transport creation option
type Option func(kcp *kcpTransport) error

// WithTLS create kcp transport with TLS
func WithTLS() Option {
	return func(kcp *kcpTransport) error {
		identity, err := tlsp2p.NewIdentity(kcp.privKey)

		if err != nil {
			return errors.Wrap(err, "generate identity from private key error")
		}

		kcp.identity = identity

		return nil
	}
}

type kcpTransport struct {
	slf4go.Logger                  // mixin logger
	localPeer     peer.ID          // local peer.ID
	privKey       crypto.PrivKey   // local peer key
	identity      *tlsp2p.Identity //
}

// New create kcp transport
func New(privkey crypto.PrivKey, options ...Option) (transport.Transport, error) {

	id, err := peer.IDFromPrivateKey(privkey)

	if err != nil {
		return nil, errors.Wrap(err, "generate peer id  from private key error")
	}

	kcp := &kcpTransport{
		Logger:    slf4go.Get("kcp-transport"),
		localPeer: id,
		privKey:   privkey,
	}

	for _, option := range options {
		if err := option(kcp); err != nil {
			return nil, err
		}
	}

	return kcp, nil
}

func smuxConf() (conf *smux.Config) {
	conf = smux.DefaultConfig()
	// TODO: potentially tweak timeouts
	conf.KeepAliveInterval = time.Second * 5
	conf.KeepAliveTimeout = time.Second * 13
	return
}

func (kcp *kcpTransport) Dial(ctx context.Context, raddr multiaddr.Multiaddr, p peer.ID) (transport.CapableConn, error) {
	kcp.I("dial to {@addr}", raddr)

	var remotePubKey crypto.PubKey

	network, host, err := manet.DialArgs(raddr)

	if err != nil {
		return nil, errors.Wrap(err, "manet.DialArgs error")
	}

	addr, err := net.ResolveUDPAddr(network, host)

	if err != nil {
		return nil, errors.Wrap(err, "resolve udp addr %s %s error", network, host)
	}

	kcpConn, err := kcpgo.Dial(addr.String())

	if err != nil {
		return nil, errors.Wrap(err, "kcp dial to %s error", addr.String())
	}

	if kcp.identity != nil {
		tlsConf, keyCh := kcp.identity.ConfigForPeer(p)

		tlsConn := tls.Client(kcpConn, tlsConf)

		// explicit call handshake
		err = tlsConn.Handshake()

		if err != nil {
			return nil, errors.Wrap(err, "kcp dial to %s tls handshake error", addr.String())
		}

		select {
		case remotePubKey = <-keyCh:
		default:
		}

		if remotePubKey == nil {
			return nil, errors.Wrap(ErrTLS, "connect to %s error", p.Pretty())
		}

		kcpConn = tlsConn
	}

	remoteMultiaddr, err := toKcpMultiaddr(addr)

	if err != nil {
		return nil, errors.Wrap(err, "create remote multiaddr error")
	}

	localMultiaddr, err := toKcpMultiaddr(kcpConn.LocalAddr())

	if err != nil {
		return nil, errors.Wrap(err, "create local multiaddr error")
	}

	smuxSession, err := smux.Client(kcpConn, smuxConf())

	if err != nil {
		return nil, errors.Wrap(err, "create kcp smux session error")
	}

	return &kcpCapableConn{
		kcp:             kcp,
		conn:            kcpConn,
		localMultiaddr:  localMultiaddr,
		remoteMultiaddr: remoteMultiaddr,
		remotePeerID:    p,
		localPeer:       kcp.localPeer,
		privKey:         kcp.privKey,
		session:         smuxSession,
		remotePubKey:    remotePubKey,
	}, nil
}

func (kcp *kcpTransport) CanDial(addr multiaddr.Multiaddr) bool {

	_, err := fromKcpMultiaddr(addr)

	return err == nil
}

func (kcp *kcpTransport) Listen(laddr multiaddr.Multiaddr) (transport.Listener, error) {
	kcp.I("listen on {@addr}", laddr)

	network, host, err := manet.DialArgs(laddr)

	if err != nil {
		return nil, errors.Wrap(err, "manet.DialArgs error")
	}

	addr, err := net.ResolveUDPAddr(network, host)

	if err != nil {
		return nil, err
	}

	listener, err := kcpgo.Listen(addr.String())

	if err != nil {
		return nil, errors.Wrap(err, "listen %s error", addr.String())
	}

	l := &kcpListener{
		listener:       listener,
		localMultiaddr: laddr,
		transport:      kcp,
		privKey:        kcp.privKey,
		localPeer:      kcp.localPeer,
	}

	if kcp.identity != nil {
		var tlsConf tls.Config

		tlsConf.GetConfigForClient = func(_ *tls.ClientHelloInfo) (*tls.Config, error) {
			// return a tls.Config that verifies the peer's certificate chain.
			// Note that since we have no way of associating an incoming QUIC connection with
			// the peer ID calculated here, we don't actually receive the peer's public key
			// from the key chan.
			conf, _ := kcp.identity.ConfigForAny()
			return conf, nil
		}

		l.tlsConf = &tlsConf
	}

	return l, nil
}

func (kcp *kcpTransport) Protocols() []int {
	return []int{protocolKCPID}
}

func (kcp *kcpTransport) Proxy() bool {
	return false
}

func (kcp *kcpTransport) String() string {
	return "kcp"
}

var kcpMultiAddr multiaddr.Multiaddr

func init() {
	var err error
	kcpMultiAddr, err = multiaddr.NewMultiaddr("/kcp")
	if err != nil {
		panic(err)
	}
}

func toKcpMultiaddr(na net.Addr) (multiaddr.Multiaddr, error) {
	udpMA, err := manet.FromNetAddr(na)
	if err != nil {
		return nil, err
	}
	return udpMA.Encapsulate(kcpMultiAddr), nil
}

func fromKcpMultiaddr(addr multiaddr.Multiaddr) (net.Addr, error) {
	return manet.ToNetAddr(addr.Decapsulate(kcpMultiAddr))
}

type kcpCapableConn struct {
	kcp            *kcpTransport
	conn           net.Conn
	localPeer      peer.ID
	privKey        crypto.PrivKey
	localMultiaddr multiaddr.Multiaddr

	remotePeerID    peer.ID
	remotePubKey    crypto.PubKey
	remoteMultiaddr multiaddr.Multiaddr
	session         *smux.Session
}

func (c *kcpCapableConn) Close() error {
	return nil
}

// IsClosed returns whether a connection is fully closed.
func (c *kcpCapableConn) IsClosed() bool {
	return false
}

// OpenStream creates a new stream.
func (c *kcpCapableConn) OpenStream() (mux.MuxedStream, error) {

	c.kcp.D("open stream {@c} -- start", c.localPeer.Pretty())

	stream, err := c.session.OpenStream()

	if err != nil {
		return nil, errors.Wrap(err, "open kcp smux session error")
	}

	c.kcp.D("open stream {@c} -- finish", c.localPeer.Pretty())

	return &kcpStream{Stream: stream}, nil
}

// AcceptStream accepts a stream opened by the other side.
func (c *kcpCapableConn) AcceptStream() (mux.MuxedStream, error) {

	c.kcp.D("accept stream {@c} -- start", c.localPeer.Pretty())

	stream, err := c.session.AcceptStream()

	if err != nil {
		return nil, errors.Wrap(err, "open kcp smux session error")
	}

	c.kcp.D("accept stream {@c} -- finish", c.localPeer.Pretty())

	return &kcpStream{Stream: stream}, nil
}

// LocalPeer returns our peer ID
func (c *kcpCapableConn) LocalPeer() peer.ID {
	return c.localPeer
}

// LocalPrivateKey returns our private key
func (c *kcpCapableConn) LocalPrivateKey() crypto.PrivKey {
	return c.privKey
}

// RemotePeer returns the peer ID of the remote peer.
func (c *kcpCapableConn) RemotePeer() peer.ID {
	return c.remotePeerID
}

// RemotePublicKey returns the public key of the remote peer.
func (c *kcpCapableConn) RemotePublicKey() crypto.PubKey {
	return c.remotePubKey
}

// LocalMultiaddr returns the local Multiaddr associated
func (c *kcpCapableConn) LocalMultiaddr() multiaddr.Multiaddr {
	return c.localMultiaddr
}

// RemoteMultiaddr returns the remote Multiaddr associated
func (c *kcpCapableConn) RemoteMultiaddr() multiaddr.Multiaddr {
	return c.remoteMultiaddr
}

func (c *kcpCapableConn) Transport() transport.Transport {
	return c.kcp
}

type kcpListener struct {
	listener       net.Listener
	transport      *kcpTransport
	privKey        crypto.PrivKey
	localPeer      peer.ID
	localMultiaddr multiaddr.Multiaddr
	tlsConf        *tls.Config
}

// Accept accepts new connections.
func (l *kcpListener) Accept() (transport.CapableConn, error) {
	for {
		sess, err := l.listener.Accept()

		if err != nil {
			return nil, err
		}

		l.transport.D("accept connection {@raddr}", sess.RemoteAddr())

		var remotePeer peer.ID

		if l.tlsConf != nil {
			tlsSess := tls.Server(sess, l.tlsConf)

			err := tlsSess.Handshake()

			if err != nil {
				return nil, err
			}

			remotePubKey, err := tlsp2p.PubKeyFromCertChain(tlsSess.ConnectionState().PeerCertificates)

			if err != nil {
				return nil, err
			}

			remotePeer, err = peer.IDFromPublicKey(remotePubKey)

			if err != nil {
				return nil, err
			}

			sess = tlsSess
		}

		remoteMultiaddr, err := toKcpMultiaddr(sess.RemoteAddr())

		if err != nil {
			return nil, errors.Wrap(err, "parse remote multiaddr error")
		}

		smuxSession, err := smux.Server(sess, smuxConf())

		if err != nil {
			return nil, errors.Wrap(err, "create kcp smux session error")
		}

		return &kcpCapableConn{
			conn:            sess,
			kcp:             l.transport,
			localMultiaddr:  l.localMultiaddr,
			remoteMultiaddr: remoteMultiaddr,
			localPeer:       l.transport.localPeer,
			privKey:         l.transport.privKey,
			session:         smuxSession,
			remotePeerID:    remotePeer,
		}, nil
	}
}

// Close closes the listener.
func (l *kcpListener) Close() error {
	return nil
}

// Addr returns the address of this listener.
func (l *kcpListener) Addr() net.Addr {
	return l.listener.Addr()
}

// Multiaddr returns the multiaddress of this listener.
func (l *kcpListener) Multiaddr() multiaddr.Multiaddr {
	return l.localMultiaddr
}

type kcpStream struct {
	*smux.Stream
}

func (s *kcpStream) Reset() error {
	return nil
}
