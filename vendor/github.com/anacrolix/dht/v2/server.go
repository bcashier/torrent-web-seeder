package dht

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/anacrolix/log"
	"github.com/pkg/errors"

	"github.com/anacrolix/missinggo"
	"github.com/anacrolix/missinggo/conntrack"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/iplist"
	"github.com/anacrolix/torrent/logonce"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/anacrolix/dht/v2/krpc"
)

// A Server defines parameters for a DHT node server that is able to send
// queries, and respond to the ones from the network. Each node has a globally
// unique identifier known as the "node ID." Node IDs are chosen at random
// from the same 160-bit space as BitTorrent infohashes and define the
// behaviour of the node. Zero valued Server does not have a valid ID and thus
// is unable to function properly. Use `NewServer(nil)` to initialize a
// default node.
type Server struct {
	id     int160
	socket net.PacketConn

	mu           sync.RWMutex
	transactions map[transactionKey]*Transaction
	nextT        uint64 // unique "t" field for outbound queries
	table        table
	closed       missinggo.Event
	ipBlockList  iplist.Ranger
	tokenServer  tokenServer // Manages tokens we issue to our queriers.
	config       ServerConfig
	stats        ServerStats
}

func (s *Server) numGoodNodes() (num int) {
	s.table.forNodes(func(n *node) bool {
		if n.IsGood() {
			num++
		}
		return true
	})
	return
}

func prettySince(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	d /= time.Second
	d *= time.Second
	return fmt.Sprintf("%s ago", d)
}

func (s *Server) WriteStatus(w io.Writer) {
	fmt.Fprintf(w, "Listening on %s\n", s.Addr())
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(w, "Nodes in table: %d good, %d total\n", s.numGoodNodes(), s.numNodes())
	fmt.Fprintf(w, "Ongoing transactions: %d\n", len(s.transactions))
	fmt.Fprintf(w, "Server node ID: %x\n", s.id.Bytes())
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 0, 1, ' ', 0)
	fmt.Fprintf(tw, "b#\tnode id\taddr\tanntok\tlast query\tlast response\tcf\n")
	for i, b := range s.table.buckets {
		b.EachNode(func(n *node) bool {
			fmt.Fprintf(tw, "%d\t%x\t%s\t%v\t%s\t%s\t%d\n",
				i,
				n.id.Bytes(),
				n.addr,
				func() int {
					if n.announceToken == nil {
						return -1
					}
					return len(*n.announceToken)
				}(),
				prettySince(n.lastGotQuery),
				prettySince(n.lastGotResponse),
				n.consecutiveFailures,
			)
			return true
		})
	}
	tw.Flush()
	fmt.Fprintln(w)
}

func (s *Server) numNodes() (num int) {
	s.table.forNodes(func(n *node) bool {
		num++
		return true
	})
	return
}

// Stats returns statistics for the server.
func (s *Server) Stats() ServerStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.stats
	ss.GoodNodes = s.numGoodNodes()
	ss.Nodes = s.numNodes()
	ss.OutstandingTransactions = len(s.transactions)
	return ss
}

// Addr returns the listen address for the server. Packets arriving to this address
// are processed by the server (unless aliens are involved).
func (s *Server) Addr() net.Addr {
	return s.socket.LocalAddr()
}

// NewServer initializes a new DHT node server.
func NewServer(c *ServerConfig) (s *Server, err error) {
	if c == nil {
		c = &ServerConfig{
			Conn:               mustListen(":0"),
			NoSecurity:         true,
			StartingNodes:      GlobalBootstrapAddrs,
			ConnectionTracking: conntrack.NewInstance(),
		}
	}
	if c.Conn == nil {
		return nil, errors.New("non-nil Conn required")
	}
	if missinggo.IsZeroValue(c.NodeId) {
		c.NodeId = RandomNodeID()
		if !c.NoSecurity && c.PublicIP != nil {
			SecureNodeId(&c.NodeId, c.PublicIP)
		}
	}
	// If Logger is empty, emulate the old behaviour: Everything is logged to the default location,
	// and there are no debug messages.
	if c.Logger.LoggerImpl == nil {
		c.Logger = log.Default.WithFilter(func(m log.Msg) bool {
			return !m.HasValue(log.Debug)
		})
	}
	// Add log.Debug by default.
	c.Logger = c.Logger.WithMap(func(m log.Msg) log.Msg {
		var l log.Level
		if m.GetValueByType(&l) {
			return m
		}
		return m.WithValues(log.Debug)
	})

	s = &Server{
		config:      *c,
		ipBlockList: c.IPBlocklist,
		tokenServer: tokenServer{
			maxIntervalDelta: 2,
			interval:         5 * time.Minute,
			secret:           make([]byte, 20),
		},
		transactions: make(map[transactionKey]*Transaction),
		table: table{
			k: 8,
		},
	}
	if s.config.ConnectionTracking == nil {
		s.config.ConnectionTracking = conntrack.NewInstance()
	}
	rand.Read(s.tokenServer.secret)
	s.socket = c.Conn
	s.id = int160FromByteArray(c.NodeId)
	s.table.rootID = s.id
	go s.serveUntilClosed()
	return
}

func (s *Server) serveUntilClosed() {
	err := s.serve()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.IsSet() {
		return
	}
	if err != nil {
		panic(err)
	}
}

// Returns a description of the Server. Python repr-style.
func (s *Server) String() string {
	return fmt.Sprintf("dht server on %s", s.socket.LocalAddr())
}

// Packets to and from any address matching a range in the list are dropped.
func (s *Server) SetIPBlockList(list iplist.Ranger) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ipBlockList = list
}

func (s *Server) IPBlocklist() iplist.Ranger {
	return s.ipBlockList
}

func (s *Server) processPacket(b []byte, addr Addr) {
	if len(b) < 2 || b[0] != 'd' {
		// KRPC messages are bencoded dicts.
		readNotKRPCDict.Add(1)
		return
	}
	var d krpc.Msg
	err := bencode.Unmarshal(b, &d)
	if _, ok := err.(bencode.ErrUnusedTrailingBytes); ok {
		// log.Printf("%s: received message packet with %d trailing bytes: %q", s, _err.NumUnusedBytes, b[len(b)-_err.NumUnusedBytes:])
		expvars.Add("processed packets with trailing bytes", 1)
	} else if err != nil {
		readUnmarshalError.Add(1)
		func() {
			if se, ok := err.(*bencode.SyntaxError); ok {
				// The message was truncated.
				if int(se.Offset) == len(b) {
					return
				}
				// Some messages seem to drop to nul chars abrubtly.
				if int(se.Offset) < len(b) && b[se.Offset] == 0 {
					return
				}
				// The message isn't bencode from the first.
				if se.Offset == 0 {
					return
				}
			}
			// if missinggo.CryHeard() {
			// 	log.Printf("%s: received bad krpc message from %s: %s: %+q", s, addr, err, b)
			// }
		}()
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.IsSet() {
		return
	}
	var n *node
	if sid := d.SenderID(); sid != nil {
		n, _ = s.getNode(addr, int160FromByteArray(*sid), !d.ReadOnly)
		if n != nil && d.ReadOnly {
			n.readOnly = true
		}
	}
	if d.Y == "q" {
		expvars.Add("received queries", 1)
		s.logger().Printf("received query %q from %v", d.Q, addr)
		s.handleQuery(addr, d)
		return
	}
	t := s.findResponseTransaction(d.T, addr)
	if t == nil {
		s.logger().Printf("received response for untracked transaction %q from %v", d.T, addr)
		return
	}
	s.logger().Printf("received response for transaction %q from %v", d.T, addr)
	go t.handleResponse(d)
	if n != nil {
		n.lastGotResponse = time.Now()
		n.consecutiveFailures = 0
	}
	s.deleteTransaction(t)
}

func (s *Server) serve() error {
	var b [0x10000]byte
	for {
		n, addr, err := s.socket.ReadFrom(b[:])
		if err != nil {
			return err
		}
		expvars.Add("packets read", 1)
		if n == len(b) {
			logonce.Stderr.Printf("received dht packet exceeds buffer size")
			continue
		}
		if missinggo.AddrPort(addr) == 0 {
			readZeroPort.Add(1)
			continue
		}
		s.mu.Lock()
		blocked := s.ipBlocked(missinggo.AddrIP(addr))
		s.mu.Unlock()
		if blocked {
			readBlocked.Add(1)
			continue
		}
		s.processPacket(b[:n], NewAddr(addr))
	}
}

func (s *Server) ipBlocked(ip net.IP) (blocked bool) {
	if s.ipBlockList == nil {
		return
	}
	_, blocked = s.ipBlockList.Lookup(ip)
	return
}

// Adds directly to the node table.
func (s *Server) AddNode(ni krpc.NodeInfo) error {
	id := int160FromByteArray(ni.ID)
	if id.IsZero() {
		return s.Ping(ni.Addr.UDP(), nil)
	}
	_, err := s.getNode(NewAddr(ni.Addr.UDP()), int160FromByteArray(ni.ID), true)
	return err
}

func wantsContain(ws []krpc.Want, w krpc.Want) bool {
	for _, _w := range ws {
		if _w == w {
			return true
		}
	}
	return false
}

func shouldReturnNodes(queryWants []krpc.Want, querySource net.IP) bool {
	if len(queryWants) != 0 {
		return wantsContain(queryWants, krpc.WantNodes)
	}
	return querySource.To4() != nil
}

func shouldReturnNodes6(queryWants []krpc.Want, querySource net.IP) bool {
	if len(queryWants) != 0 {
		return wantsContain(queryWants, krpc.WantNodes6)
	}
	return querySource.To4() == nil
}

func (s *Server) makeReturnNodes(target int160, filter func(krpc.NodeAddr) bool) []krpc.NodeInfo {
	return s.closestGoodNodeInfos(8, target, filter)
}

var krpcErrMissingArguments = krpc.Error{
	Code: krpc.ErrorCodeProtocolError,
	Msg:  "missing arguments dict",
}

func (s *Server) setReturnNodes(r *krpc.Return, queryMsg krpc.Msg, querySource Addr) *krpc.Error {
	if queryMsg.A == nil {
		return &krpcErrMissingArguments
	}
	target := int160FromByteArray(queryMsg.A.InfoHash)
	if shouldReturnNodes(queryMsg.A.Want, querySource.IP()) {
		r.Nodes = s.makeReturnNodes(target, func(na krpc.NodeAddr) bool { return na.IP.To4() != nil })
	}
	if shouldReturnNodes6(queryMsg.A.Want, querySource.IP()) {
		r.Nodes6 = s.makeReturnNodes(target, func(krpc.NodeAddr) bool { return true })
	}
	return nil
}

// TODO: Probably should write error messages back to senders if something is
// wrong.
func (s *Server) handleQuery(source Addr, m krpc.Msg) {
	go func() {
		expvars.Add(fmt.Sprintf("received query %q", m.Q), 1)
		if a := m.A; a != nil {
			if a.NoSeed != 0 {
				expvars.Add("received argument noseed", 1)
			}
			if a.Scrape != 0 {
				expvars.Add("received argument scrape", 1)
			}
		}
	}()
	if m.SenderID() != nil {
		if n, _ := s.getNode(source, int160FromByteArray(*m.SenderID()), !m.ReadOnly); n != nil {
			n.lastGotQuery = time.Now()
		}
	}
	if s.config.OnQuery != nil {
		propagate := s.config.OnQuery(&m, source.Raw())
		if !propagate {
			return
		}
	}
	// Don't respond.
	if s.config.Passive {
		return
	}
	// TODO: Should we disallow replying to ourself?
	args := m.A
	switch m.Q {
	case "ping":
		s.reply(source, m.T, krpc.Return{})
	case "get_peers":
		var r krpc.Return
		// TODO: Return values.
		if err := s.setReturnNodes(&r, m, source); err != nil {
			s.sendError(source, m.T, *err)
			break
		}
		r.Token = func() *string {
			t := s.createToken(source)
			return &t
		}()
		s.reply(source, m.T, r)
	case "find_node":
		var r krpc.Return
		if err := s.setReturnNodes(&r, m, source); err != nil {
			s.sendError(source, m.T, *err)
			break
		}
		s.reply(source, m.T, r)
	case "announce_peer":
		readAnnouncePeer.Add(1)
		if !s.validToken(args.Token, source) {
			expvars.Add("received announce_peer with invalid token", 1)
			return
		}
		expvars.Add("received announce_peer with valid token", 1)
		if h := s.config.OnAnnouncePeer; h != nil {
			p := Peer{
				IP:   source.IP(),
				Port: args.Port,
			}
			if args.ImpliedPort {
				p.Port = source.Port()
			}
			go h(metainfo.Hash(args.InfoHash), p)
		}
		s.reply(source, m.T, krpc.Return{})
	default:
		s.sendError(source, m.T, krpc.ErrorMethodUnknown)
	}
}

func (s *Server) sendError(addr Addr, t string, e krpc.Error) {
	m := krpc.Msg{
		T: t,
		Y: "e",
		E: &e,
	}
	b, err := bencode.Marshal(m)
	if err != nil {
		panic(err)
	}
	s.logger().Printf("sending error to %v: %v", addr, e)
	_, err = s.writeToNode(b, addr)
	if err != nil {
		s.config.Logger.Printf("error replying to %s: %s", addr, err)
	}
}

func (s *Server) reply(addr Addr, t string, r krpc.Return) {
	expvars.Add("replied to peer", 1)
	r.ID = s.id.AsByteArray()
	m := krpc.Msg{
		T:  t,
		Y:  "r",
		R:  &r,
		IP: addr.KRPC(),
	}
	b, err := bencode.Marshal(m)
	if err != nil {
		panic(err)
	}
	log.Fmsg("replying to %v", addr).Log(s.logger())
	_, err = s.writeToNode(b, addr)
	if err != nil {
		s.config.Logger.Printf("error replying to %s: %s", addr, err)
	}
}

// Returns the node if it's in the routing table, adding it if appropriate.
func (s *Server) getNode(addr Addr, id int160, tryAdd bool) (*node, error) {
	if n := s.table.getNode(addr, id); n != nil {
		return n, nil
	}
	n := &node{nodeKey: nodeKey{
		id:   id,
		addr: addr,
	}}
	// Check that the node would be good to begin with. (It might have a bad
	// ID or banned address, or we fucked up the initial node field
	// invariant.)
	if err := s.nodeErr(n); err != nil {
		return nil, err
	}
	if !tryAdd {
		return nil, errors.New("node not present and add flag false")
	}
	b := s.table.bucketForID(id)
	if b.Len() >= s.table.k {
		if b.EachNode(func(n *node) bool {
			if s.nodeIsBad(n) {
				s.table.dropNode(n)
			}
			return b.Len() >= s.table.k
		}) {
			return nil, errors.New("no room in bucket")
		}
	}
	if err := s.table.addNode(n); err != nil {
		panic(fmt.Sprintf("expected to add node: %s", err))
	}
	return n, nil
}

func (s *Server) nodeIsBad(n *node) bool {
	return s.nodeErr(n) != nil
}

func (s *Server) nodeErr(n *node) error {
	if n.id == s.id {
		return errors.New("is self")
	}
	if n.id.IsZero() {
		return errors.New("has zero id")
	}
	if !s.config.NoSecurity && !n.IsSecure() {
		return errors.New("not secure")
	}
	if n.IsGood() {
		return nil
	}
	if n.consecutiveFailures >= 3 {
		return fmt.Errorf("has %d consecutive failures", n.consecutiveFailures)
	}
	return nil
}

func (s *Server) writeToNode(b []byte, node Addr) (wrote bool, err error) {
	if list := s.ipBlockList; list != nil {
		if r, ok := list.Lookup(node.IP()); ok {
			err = fmt.Errorf("write to %s blocked: %s", node, r.Description)
			return
		}
	}
	//s.config.Logger.WithValues(log.Debug).Printf("writing to %s: %q", node.String(), b)
	n, err := s.socket.WriteTo(b, node.Raw())
	writes.Add(1)
	if err != nil {
		writeErrors.Add(1)
		err = fmt.Errorf("error writing %d bytes to %s: %s", len(b), node, err)
		return
	}
	wrote = true
	if n != len(b) {
		err = io.ErrShortWrite
		return
	}
	return
}

func (s *Server) findResponseTransaction(transactionID string, sourceNode Addr) *Transaction {
	return s.transactions[transactionKey{
		sourceNode.String(),
		transactionID}]
}

func (s *Server) nextTransactionID() string {
	var b [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(b[:], s.nextT)
	s.nextT++
	return string(b[:n])
}

func (s *Server) deleteTransaction(t *Transaction) {
	delete(s.transactions, t.key())
}

func (s *Server) deleteTransactionUnlocked(t *Transaction) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deleteTransaction(t)
}

func (s *Server) addTransaction(t *Transaction) {
	if _, ok := s.transactions[t.key()]; ok {
		panic("transaction not unique")
	}
	s.transactions[t.key()] = t
}

// ID returns the 20-byte server ID. This is the ID used to communicate with the
// DHT network.
func (s *Server) ID() [20]byte {
	return s.id.AsByteArray()
}

func (s *Server) createToken(addr Addr) string {
	return s.tokenServer.CreateToken(addr)
}

func (s *Server) validToken(token string, addr Addr) bool {
	return s.tokenServer.ValidToken(token, addr)
}

func (s *Server) connTrackEntryForAddr(a Addr) conntrack.Entry {
	return conntrack.Entry{
		s.socket.LocalAddr().Network(),
		s.socket.LocalAddr().String(),
		a.String(),
	}
}

func (s *Server) query(addr Addr, q string, a *krpc.MsgArgs, callback func(krpc.Msg, error)) error {
	if callback == nil {
		callback = func(krpc.Msg, error) {}
	}
	go func() {
		callback(s.queryContext(context.Background(), addr, q, a))
	}()
	return nil
}

func (s *Server) queryContext(ctx context.Context, addr Addr, q string, a *krpc.MsgArgs) (reply krpc.Msg, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tid := s.nextTransactionID()
	if a == nil {
		a = &krpc.MsgArgs{}
	}
	a.ID = s.ID()
	m := krpc.Msg{
		T: tid,
		Y: "q",
		Q: q,
		A: a,
	}
	// BEP 43. Outgoing queries from passive nodes should contain "ro":1 in
	// the top level dictionary.
	if s.config.Passive {
		m.ReadOnly = true
	}
	b, err := bencode.Marshal(m)
	if err != nil {
		return
	}
	replyChan := make(chan krpc.Msg, 1)
	errChan := make(chan error, 1)
	t := &Transaction{
		remoteAddr: addr,
		t:          tid,
		q:          q,
		querySender: func(attempt int) error {
			s.logger().Printf("sending query %q to %v (attempt %d/%d)", q, addr, attempt, maxTransactionSends)
			cteh := s.config.ConnectionTracking.Wait(ctx, s.connTrackEntryForAddr(addr), "send dht query", -1)
			wrote, err := s.writeToNode(b, addr)
			if wrote {
				cteh.Done()
			} else {
				cteh.Forget()
			}
			return err
		},
		onResponse: func(m krpc.Msg) {
			replyChan <- m
		},
		onTimeout: func() {
			errChan <- errors.New("query timed out")
		},
		onSendError: func(err error) {
			errChan <- fmt.Errorf("error sending query: %s", err)
		},
		queryResendDelay: func() time.Duration {
			if s.config.QueryResendDelay != nil {
				return s.config.QueryResendDelay()
			}
			return defaultQueryResendDelay()
		},
	}
	s.stats.OutboundQueriesAttempted++
	t.mu.Lock()
	t.startResendTimer()
	t.mu.Unlock()
	s.addTransaction(t)
	defer func() {
		if err != nil {
			for _, n := range s.table.addrNodes(addr) {
				n.consecutiveFailures++
			}
		}
	}()
	defer s.deleteTransaction(t)
	s.mu.Unlock()
	go expvars.Add(fmt.Sprintf("outbound %s queries", q), 1)
	defer s.mu.Lock()
	select {
	case reply = <-replyChan:
		return
	case <-ctx.Done():
		err = ctx.Err()
		return
	case err = <-errChan:
		return
	}
}

// Sends a ping query to the address given.
func (s *Server) Ping(node *net.UDPAddr, callback func(krpc.Msg, error)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ping(node, callback)
}

func (s *Server) ping(node *net.UDPAddr, callback func(krpc.Msg, error)) error {
	return s.query(NewAddr(node), "ping", nil, callback)
}

func (s *Server) announcePeer(node Addr, infoHash int160, port int, token string, impliedPort bool, callback func(krpc.Msg, error)) error {
	if port == 0 && !impliedPort {
		return errors.New("nothing to announce")
	}
	return s.query(node, "announce_peer", &krpc.MsgArgs{
		ImpliedPort: impliedPort,
		InfoHash:    infoHash.AsByteArray(),
		Port:        port,
		Token:       token,
	}, func(m krpc.Msg, err error) {
		if callback != nil {
			go callback(m, err)
		}
		if err := m.Error(); err != nil {
			announceErrors.Add(1)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.stats.SuccessfulOutboundAnnouncePeerQueries++
	})
}

// Add response nodes to node table.
func (s *Server) addResponseNodes(d krpc.Msg) {
	if d.R == nil {
		return
	}
	d.R.ForAllNodes(func(ni krpc.NodeInfo) {
		s.getNode(NewAddr(ni.Addr.UDP()), int160FromByteArray(ni.ID), true)
	})
}

// Sends a find_node query to addr. targetID is the node we're looking for.
func (s *Server) findNode(addr Addr, targetID int160, callback func(krpc.Msg, error)) (err error) {
	return s.query(addr, "find_node", &krpc.MsgArgs{
		Target: targetID.AsByteArray(),
		Want:   []krpc.Want{krpc.WantNodes, krpc.WantNodes6},
	}, func(m krpc.Msg, err error) {
		// Scrape peers from the response to put in the server's table before
		// handing the response back to the caller.
		s.mu.Lock()
		s.addResponseNodes(m)
		s.mu.Unlock()
		callback(m, err)
	})
}

type TraversalStats struct {
	NumAddrsTried int
	NumResponses  int
}

func (me TraversalStats) String() string {
	return fmt.Sprintf("%#v", me)
}

// Populates the node table.
func (s *Server) Bootstrap() (ts TraversalStats, err error) {
	initialAddrs, err := s.traversalStartingNodes()
	if err != nil {
		return
	}
	var outstanding sync.WaitGroup
	triedAddrs := newBloomFilterForTraversal()
	var onAddr func(addr Addr)
	onAddr = func(addr Addr) {
		if triedAddrs.Test([]byte(addr.String())) {
			return
		}
		ts.NumAddrsTried++
		outstanding.Add(1)
		triedAddrs.AddString(addr.String())
		s.findNode(addr, s.id, func(m krpc.Msg, err error) {
			defer outstanding.Done()
			s.mu.Lock()
			defer s.mu.Unlock()
			if err != nil {
				return
			}
			ts.NumResponses++
			if r := m.R; r != nil {
				r.ForAllNodes(func(ni krpc.NodeInfo) {
					onAddr(NewAddr(ni.Addr.UDP()))
				})
			}
		})
	}
	s.mu.Lock()
	for _, addr := range initialAddrs {
		onAddr(NewAddr(addr.Addr.UDP()))
	}
	s.mu.Unlock()
	outstanding.Wait()
	return
}

// Returns how many nodes are in the node table.
func (s *Server) NumNodes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.numNodes()
}

// Exports the current node table.
func (s *Server) Nodes() (nis []krpc.NodeInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.table.forNodes(func(n *node) bool {
		nis = append(nis, krpc.NodeInfo{
			Addr: n.addr.KRPC(),
			ID:   n.id.AsByteArray(),
		})
		return true
	})
	return
}

// Stops the server network activity. This is all that's required to clean-up a Server.
func (s *Server) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed.Set()
	s.socket.Close()
}

func (s *Server) getPeers(ctx context.Context, addr Addr, infoHash int160) (krpc.Msg, error) {
	m, err := s.queryContext(ctx, addr, "get_peers", &krpc.MsgArgs{
		InfoHash: infoHash.AsByteArray(),
		Want:     []krpc.Want{krpc.WantNodes, krpc.WantNodes6},
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addResponseNodes(m)
	if m.R != nil {
		if m.R.Token == nil {
			expvars.Add("get_peers responses with no token", 1)
		} else if len(*m.R.Token) == 0 {
			expvars.Add("get_peers responses with empty token", 1)
		} else {
			expvars.Add("get_peers responses with token", 1)
		}
		if m.SenderID() != nil && m.R.Token != nil {
			if n, _ := s.getNode(addr, int160FromByteArray(*m.SenderID()), false); n != nil {
				n.announceToken = m.R.Token
			}
		}
	}
	return m, err
}

func (s *Server) closestGoodNodeInfos(
	k int,
	targetID int160,
	filter func(krpc.NodeAddr) bool,
) (
	ret []krpc.NodeInfo,
) {
	for _, n := range s.closestNodes(k, targetID, func(n *node) bool {
		return n.IsGood() && filter(n.NodeInfo().Addr)
	}) {
		ret = append(ret, n.NodeInfo())
	}
	return
}

func (s *Server) closestNodes(k int, target int160, filter func(*node) bool) []*node {
	return s.table.closestNodes(k, target, filter)
}

func (s *Server) traversalStartingNodes() (nodes []addrMaybeId, err error) {
	s.mu.RLock()
	s.table.forNodes(func(n *node) bool {
		nodes = append(nodes, addrMaybeId{n.addr.KRPC(), &n.id})
		return true
	})
	s.mu.RUnlock()
	if len(nodes) > 0 {
		return
	}
	if s.config.StartingNodes != nil {
		addrs, err := s.config.StartingNodes()
		if err != nil {
			return nil, errors.Wrap(err, "getting starting nodes")
		}
		for _, a := range addrs {
			nodes = append(nodes, addrMaybeId{a.KRPC(), nil})
		}
	}
	if len(nodes) == 0 {
		err = errors.New("no initial nodes")
	}
	return
}

func (s *Server) AddNodesFromFile(fileName string) (added int, err error) {
	ns, err := ReadNodesFromFile(fileName)
	if err != nil {
		return
	}
	for _, n := range ns {
		if s.AddNode(n) == nil {
			added++
		}
	}
	return
}

func (s *Server) logger() log.Logger {
	return s.config.Logger
}
