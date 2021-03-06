package main

import (
	"os"
	"fmt"
	"net"
	"time"
	"sort"
	"bytes"
	"errors"
	"strings"
	"crypto/rand"
	"encoding/binary"
	"github.com/piotrnar/gocoin/btc"
)


const (
	Version = 70001
	UserAgent = "/Satoshi:0.8.1/"

	Services = uint64(0x1)

	SendAddrsEvery = (15*60) // 15 minutes

	MaxInCons = 24
	MaxOutCons = 8
	MaxTotCons = MaxInCons+MaxOutCons

	NoDataTimeout = 60
)


var (
	openCons map[uint64]*oneConnection = make(map[uint64]*oneConnection, MaxTotCons)
	InvsSent, BlockSent uint64
	InConsActive, OutConsActive uint
	
	DefaultTcpPort uint16
	MyExternalAddr *btc.NetAddr
)

type oneConnection struct {
	addr *onePeer

	last_cmd string
	
	broken bool // maker that the conenction has been broken
	ban bool // ban this client after disconnecting
	writing bool // we are currently busy writing to the socket

	listen bool
	*net.TCPConn
	
	connectedAt int64
	ver_ack_received bool

	hdr [24]byte
	hdr_len int

	dat []byte
	datlen uint32

	invs2send []*[36]byte

	BytesReceived, BytesSent uint64

	// Data from the version message
	node struct {
		version uint32
		services uint64
		timestamp uint64
		height uint32
		agent string
	}

	NextAddrSent uint32 // When we shoudl annonce our "addr" again

	LastDataGot uint32 // if we have no data for some time, we abort this conenction
}


type BCmsg struct {
	cmd string
	pl  []byte
}


func (c *oneConnection) SendRawMsg(cmd string, pl []byte) (e error) {
	var b [20]byte

	c.last_cmd = cmd+"*"
	c.writing = true

	binary.LittleEndian.PutUint32(b[0:4], Version)
	copy(b[0:4], Magic[:])
	copy(b[4:16], cmd)
	binary.LittleEndian.PutUint32(b[16:20], uint32(len(pl)))
	e = SockWrite(c.TCPConn, b[:20])
	if e != nil {
		if dbg > 0 {
			println("SendRawMsg 1", e.Error())
		}
		c.broken = true
		c.writing = false
		return
	}

	sh := btc.Sha2Sum(pl[:])
	e = SockWrite(c.TCPConn, sh[:4])
	if e != nil {
		if dbg > 0 {
			println("SendRawMsg 2", e.Error())
		}
		c.broken = true
		c.writing = false
		return
	}

	e = SockWrite(c.TCPConn, pl[:])
	if e != nil {
		if dbg > 0 {
			println("SendRawMsg 3", e.Error())
		}
		c.broken = true
	}

	c.BytesSent += uint64(24+len(pl))
	c.last_cmd = cmd

	c.writing = false
	return
}


func (c *oneConnection) DoS() {
	c.ban = true
	c.broken = true
}


func putaddr(b *bytes.Buffer, a string) {
	var ip [4]byte
	var p uint16
	n, e := fmt.Sscanf(a, "%d.%d.%d.%d:%d", &ip[0], &ip[1], &ip[2], &ip[3], &p)
	if e != nil || n != 5 {
		println("Incorrect address:", a)
		os.Exit(1)
	}
	binary.Write(b, binary.LittleEndian, uint64(Services))
	// No Ip6 supported:
	b.Write([]byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xFF, 0xFF})
	b.Write(ip[:])
	binary.Write(b, binary.BigEndian, uint16(p))
}


func (c *oneConnection) SendVersion() {
	b := bytes.NewBuffer([]byte{})

	binary.Write(b, binary.LittleEndian, uint32(Version))
	binary.Write(b, binary.LittleEndian, uint64(Services))
	binary.Write(b, binary.LittleEndian, uint64(time.Now().Unix()))

	putaddr(b, c.TCPConn.RemoteAddr().String())
	putaddr(b, c.TCPConn.LocalAddr().String())

	var nonce [8]byte
	rand.Read(nonce[:])
	b.Write(nonce[:])

	b.WriteByte(byte(len(UserAgent)))
	b.Write([]byte(UserAgent))

	binary.Write(b, binary.LittleEndian, uint32(LastBlock.Height))

	c.SendRawMsg("version", b.Bytes())
}


func (c *oneConnection) HandleError(e error) (error) {
	if nerr, ok := e.(net.Error); ok && nerr.Timeout() {
		//fmt.Println("Just a timeout - ignore")
		return nil
	}
	if dbg>0 {
		println("HandleError:", e.Error())
	}
	c.hdr_len = 0
	c.dat = nil
	c.broken = true
	return e
}


func (c *oneConnection) FetchMessage() (*BCmsg) {
	var e error
	var n int

	// Try for 1 millisecond and timeout if full msg not received
	c.TCPConn.SetReadDeadline(time.Now().Add(time.Millisecond))

	for c.hdr_len < 24 {
		n, e = SockRead(c.TCPConn, c.hdr[c.hdr_len:24])
		c.hdr_len += n
		if e != nil {
			c.HandleError(e)
			return nil
		}
		if c.hdr_len>=4 && !bytes.Equal(c.hdr[:4], Magic[:]) {
			println("FetchMessage: Proto out of sync")
			c.broken = true
			return nil
		}
	}

	dlen :=  binary.LittleEndian.Uint32(c.hdr[16:20])
	if dlen > 0 {
		if c.dat == nil {
			c.dat = make([]byte, dlen)
			c.datlen = 0
		}
		for c.datlen < dlen {
			n, e = SockRead(c.TCPConn, c.dat[c.datlen:])
			c.datlen += uint32(n)
			if e != nil {
				c.HandleError(e)
				return nil
			}
		}
	}

	sh := btc.Sha2Sum(c.dat)
	if !bytes.Equal(c.hdr[20:24], sh[:4]) {
		println("Msg checksum error")
		c.hdr_len = 0
		c.dat = nil
		c.broken = true
		return nil
	}

	ret := new(BCmsg)
	ret.cmd = strings.TrimRight(string(c.hdr[4:16]), "\000")
	ret.pl = c.dat
	c.dat = nil
	c.hdr_len = 0

	c.BytesReceived += uint64(24+len(ret.pl))

	return ret
}


func (c *oneConnection) AnnounceOwnAddr() {
	var buf [31]byte
	now := uint32(time.Now().Unix())
	c.NextAddrSent = now+SendAddrsEvery
	buf[0] = 1 // Only one address
	binary.LittleEndian.PutUint32(buf[1:5], now)
	ipd := MyExternalAddr.Bytes()
	copy(buf[5:], ipd[:])
	c.SendRawMsg("addr", buf[:])
}


func (c *oneConnection) VerMsg(pl []byte) error {
	if len(pl) >= 46 {
		c.node.version = binary.LittleEndian.Uint32(pl[0:4])
		c.node.services = binary.LittleEndian.Uint64(pl[4:12])
		c.node.timestamp = binary.LittleEndian.Uint64(pl[12:20])
		if MyExternalAddr == nil {
			MyExternalAddr = btc.NewNetAddr(pl[20:46]) // These bytes should know our external IP
			MyExternalAddr.Port = DefaultTcpPort
		}
		if len(pl) >= 86 {
			//fmt.Println("From:", btc.NewNetAddr(pl[46:72]).String())
			//fmt.Println("Nonce:", hex.EncodeToString(pl[72:80]))
			le, of := btc.VLen(pl[80:])
			of += 80
			c.node.agent = string(pl[of:of+le])
			of += le
			if len(pl) >= of+4 {
				c.node.height = binary.LittleEndian.Uint32(pl[of:of+4])
				/*of += 4
				if len(pl) >= of+1 {
					fmt.Println("Relay:", pl[of])
				}*/
			}
		}
	} else {
		return errors.New("Version message too short")
	}
	c.SendRawMsg("verack", []byte{})
	if c.listen {
		c.SendVersion()
	}
	return nil
}


func (c *oneConnection) GetBlocks(lastbl []byte) {
	if dbg > 0 {
		println("GetBlocks since", btc.NewUint256(lastbl).String())
	}
	var b [4+1+32+32]byte
	binary.LittleEndian.PutUint32(b[0:4], Version)
	b[4] = 1 // only one locator
	copy(b[5:37], lastbl)
	// the remaining bytes should be filled with zero
	c.SendRawMsg("getblocks", b[:])
}


func (c *oneConnection) ProcessInv(pl []byte) {
	if len(pl) < 37 {
		println("inv payload too short")
		return
	}
	
	cnt, of := btc.VLen(pl)
	if len(pl) != of + 36*cnt {
		println("inv payload length mismatch", len(pl), of, cnt)
	}

	var blocks2get [][32]byte
	var txs uint32
	for i:=0; i<cnt; i++ {
		typ := binary.LittleEndian.Uint32(pl[of:of+4])
		if typ==2 {
			if InvsNotify(pl[of+4:of+36]) {
				var inv [32]byte
				copy(inv[:], pl[of+4:of+36])
				blocks2get = append(blocks2get, inv)
			}
		} else {
			txs++
		}
		of+= 36
	}
	if dbg>1 {
		println(c.addr.Ip(), "ProcessInv:", cnt, "tot /", txs, "txs -> get", len(blocks2get), "blocks")
	}
	
	if len(blocks2get) > 0 {
		msg := make([]byte, 9/*maxvlen*/+len(blocks2get)*36)
		le := btc.PutVlen(msg, len(blocks2get))
		for i := range blocks2get {
			binary.LittleEndian.PutUint32(msg[le:le+4], 2)
			copy(msg[le+4:le+36], blocks2get[i][:])
			le += 36
		}
		if dbg>0 {
			println("getdata for", len(blocks2get), "/", cnt, "blocks", le)
		}
		c.SendRawMsg("getdata", msg[:le])
	}
	return
}


func addInvBlockBranch(inv map[[32]byte] bool, bl *btc.BlockTreeNode, stop *btc.Uint256) {
	if len(inv)>=500 || bl.BlockHash.Equal(stop) {
		return
	}
	inv[bl.BlockHash.Hash] = true
	for i := range bl.Childs {
		if len(inv)>=500 {
			return
		}
		addInvBlockBranch(inv, bl.Childs[i], stop)
	}
}


func (c *oneConnection) ProcessGetBlocks(pl []byte) {
	b := bytes.NewReader(pl)
	var ver uint32
	e := binary.Read(b, binary.LittleEndian, &ver)
	if e != nil {
		println("ProcessGetBlocks:", e.Error(), c.addr.Ip())
		return
	}
	cnt, e := btc.ReadVLen(b)
	if e != nil {
		println("ProcessGetBlocks:", e.Error(), c.addr.Ip())
		return
	}
	h2get := make([]*btc.Uint256, cnt)
	var h [32]byte
	for i:=0; i<int(cnt); i++ {
		n, _ := b.Read(h[:])
		if n != 32 {
			println("getblocks too short", c.addr.Ip())
			return
		}
		h2get[i] = btc.NewUint256(h[:])
		if dbg>1 {
			println(c.addr.Ip(), "getbl", h2get[i].String())
		}
	}
	n, _ := b.Read(h[:])
	if n != 32 {
		println("getblocks does not have hash_stop", c.addr.Ip())
		return
	}
	hashstop := btc.NewUint256(h[:])

	var maxheight uint32
	invs := make(map[[32]byte] bool, 500)
	for i := range h2get {
		BlockChain.BlockIndexAccess.Lock()
		if bl, ok := BlockChain.BlockIndex[h2get[i].BIdx()]; ok {
			if bl.Height > maxheight {
				maxheight = bl.Height
			}
			addInvBlockBranch(invs, bl, hashstop)
		}
		BlockChain.BlockIndexAccess.Unlock()
		if len(invs)>=500 {
			break
		}
	}
	inv := new(bytes.Buffer)
	btc.WriteVlen(inv, uint32(len(invs)))
	for k, _ := range invs {
		binary.Write(inv, binary.LittleEndian, uint32(2))
		inv.Write(k[:])
	}
	if dbg>1 {
		fmt.Println(c.addr.Ip(), "getblocks", cnt, maxheight, " ...", len(invs), "invs in resp ->", len(inv.Bytes()))
	}
	InvsSent++
	c.SendRawMsg("inv", inv.Bytes())
}


func (c *oneConnection) ProcessGetData(pl []byte) {
	//println(c.addr.Ip(), "getdata")
	b := bytes.NewReader(pl)
	cnt, e := btc.ReadVLen(b)
	if e != nil {
		println("ProcessGetData:", e.Error(), c.addr.Ip())
		return
	}
	for i:=0; i<int(cnt); i++ {
		var typ uint32
		var h [32]byte
		
		e = binary.Read(b, binary.LittleEndian, &typ)
		if e != nil {
			println("ProcessGetData:", e.Error(), c.addr.Ip())
			return
		}

		n, _ := b.Read(h[:])
		if n!=32 {
			println("ProcessGetData: pl too short", c.addr.Ip())
			return
		}

		if typ == 2 {
			uh := btc.NewUint256(h[:])
			bl, _, er := BlockChain.Blocks.BlockGet(uh)
			if er == nil {
				BlockSent++
				c.SendRawMsg("block", bl)
			} else {
				//println("block", uh.String(), er.Error())
			}
		} else if typ == 1 {
			// transaction
			uh := btc.NewUint256(h[:])
			if tx, ok := TransactionsToSend[uh.Hash]; ok {
				c.SendRawMsg("tx", tx)
				println("sent tx to", c.addr.Ip())
			}
		} else {
			println("getdata for type", typ, "not supported yet")
		}
	}
}


func (c *oneConnection) GetBlockData(h []byte) {
	var b [1+4+32]byte
	b[0] = 1 // One inv
	b[1] = 2 // Block
	copy(b[5:37], h[:32])
	if dbg > 1 {
		println("GetBlockData", btc.NewUint256(h).String())
	}
	c.SendRawMsg("getdata", b[:])
}


func (c *oneConnection) SendInvs(i2s []*[36]byte) {
	b := new(bytes.Buffer)
	btc.WriteVlen(b, uint32(len(i2s)))
	for i := range i2s {
		b.Write((*i2s[i])[:])
	}
	//println("sending invs", len(i2s), len(b.Bytes()))
	c.SendRawMsg("inv", b.Bytes())
}


func (c *oneConnection) Tick() {
	// Need to send getblocks...?
	if tmp := blocksNeeded(); tmp != nil {
		c.GetBlocks(tmp)
		return
	}

	// Need to send getdata...?
	if tmp := blockDataNeeded(); tmp != nil {
		c.GetBlockData(tmp)
		return
	}

	// Need to send inv...?
	var i2s []*[36]byte
	mutex.Lock()
	if len(c.invs2send)>0 {
		i2s = c.invs2send
		c.invs2send = nil
	}
	mutex.Unlock()
	if i2s != nil {
		c.SendInvs(i2s)
		return
	}

	if *server && (c.NextAddrSent==0 || uint32(time.Now().Unix()) >= c.NextAddrSent) {
		c.AnnounceOwnAddr()
		return
	}
}


func do_one_connection(c *oneConnection) {
	if !c.listen {
		c.SendVersion()
	}

	c.LastDataGot = uint32(time.Now().Unix())
	for {
		cmd := c.FetchMessage()
		if c.broken {
			break
		}
		
		now := uint32(time.Now().Unix())

		if cmd==nil {
			if int(now-c.LastDataGot) > NoDataTimeout {
				println(c.addr.Ip(), "no data for", NoDataTimeout, "seconds - disconnect")
				c.broken = true
				break
			} else if c.ver_ack_received {
				c.Tick()
			}
			continue
		}
		
		c.LastDataGot = now

		c.addr.Alive()

		switch cmd.cmd {
			case "version":
				er := c.VerMsg(cmd.pl)
				if er != nil {
					println("version:", er.Error())
					c.broken = true
				} else if c.listen {
					c.SendVersion()
				}

			case "verack":
				//fmt.Println("Received Ver ACK")
				c.ver_ack_received = true

			case "inv":
				c.ProcessInv(cmd.pl)
			
			case "tx": //ParseTx(cmd.pl)
				println("tx unexpected here (now)")
				c.broken = true
			
			case "addr":
				ParseAddr(cmd.pl)
			
			case "block": //block received
				netBlockReceived(c, cmd.pl)

			case "getblocks":
				c.ProcessGetBlocks(cmd.pl)

			case "getdata":
				c.ProcessGetData(cmd.pl)

			case "getaddr":
				c.AnnounceOwnAddr()

			case "alert": // do nothing

			default:
				println(cmd.cmd, "from", c.addr.Ip())
		}
	}
	if c.ban {
		c.addr.Ban()
	}
	if dbg>0 {
		println("Disconnected from", c.addr.Ip())
	}
	c.TCPConn.Close()
}


func connectionActive(ad *onePeer) (yes bool) {
	mutex.Lock()
	_, yes = openCons[ad.UniqID()]
	mutex.Unlock()
	return
}


func start_server() {
	ad, e := net.ResolveTCPAddr("tcp4", fmt.Sprint("0.0.0.0:", DefaultTcpPort))
	if e != nil {
		println("ResolveTCPAddr", e.Error())
		return
	}

	lis, e := net.ListenTCP("tcp4", ad)
	if e != nil {
		println("ListenTCP", e.Error())
		return
	}
	defer lis.Close()

	fmt.Println("TCP server started at", ad.String())

	for {
		if InConsActive < MaxInCons {
			tc, e := lis.AcceptTCP()
			if e == nil {
				if dbg>0 {
					fmt.Println("Incomming connection from", tc.RemoteAddr().String())
				}
				ad := newIncommingPeer(tc.RemoteAddr().String())
				if ad != nil {
					conn := new(oneConnection)
					conn.connectedAt = time.Now().Unix()
					conn.addr = ad
					conn.listen = true
					conn.TCPConn = tc
					mutex.Lock()
					if _, ok := openCons[ad.UniqID()]; ok {
						fmt.Println(ad.Ip(), "already connected")
						mutex.Unlock()
					} else {
						openCons[ad.UniqID()] = conn
						InConsActive++
						mutex.Unlock()
						go func () {
							do_one_connection(conn)
							mutex.Lock()
							delete(openCons, ad.UniqID())
							InConsActive--
							mutex.Unlock()
						}()
					}
				} else {
					println("newIncommingPeer failed")
					tc.Close()
				}
			}
		} else {
			time.Sleep(250e6)
		}
	}
}


func do_network(ad *onePeer) {
	var e error
	conn := new(oneConnection)
	conn.addr = ad
	mutex.Lock()
	if _, ok := openCons[ad.UniqID()]; ok {
		fmt.Println(ad.Ip(), "already connected")
		mutex.Unlock()
		return
	}
	openCons[ad.UniqID()] = conn
	OutConsActive++
	mutex.Unlock()
	go func() {
		conn.TCPConn, e = net.DialTCP("tcp4", nil, &net.TCPAddr{
			IP: net.IPv4(ad.Ip4[0], ad.Ip4[1], ad.Ip4[2], ad.Ip4[3]),
			Port: int(ad.Port)})
		if e == nil {
			conn.connectedAt = time.Now().Unix()
			if dbg>0 {
				println("Connected to", ad.Ip())
			}
			do_one_connection(conn)
		} else {
			if dbg>0 {
				println("Could not connect to", ad.Ip())
			}
			//println(e.Error())
		}
		mutex.Lock()
		delete(openCons, ad.UniqID())
		OutConsActive--
		mutex.Unlock()
	}()
}


func network_process() {
	if *server {
		go start_server()
	}
	for {
		mutex.Lock()
		conn_cnt := OutConsActive
		mutex.Unlock()
		if conn_cnt < MaxOutCons {
			ad := getBestPeer()
			if ad != nil {
				do_network(ad)
			} else if *proxy=="" {
				println("no new peers", len(openCons), conn_cnt)
			}
		}
		time.Sleep(250e6)
	}
}

func bts2str(val uint64) string {
	if val < 1e5*1024 {
		return fmt.Sprintf("%10.1f k ", float64(val)/1024)
	}
	return fmt.Sprintf("%10.1f MB", float64(val)/(1024*1024))
}


func NetSendInv(typ uint32, h []byte, fromConn *oneConnection) (cnt uint) {
	inv := new([36]byte)
	
	binary.LittleEndian.PutUint32(inv[0:4], typ)
	copy(inv[4:36], h)
	
	mutex.Lock()
	for _, v := range openCons {
		if v != fromConn && len(v.invs2send)<500 {
			v.invs2send = append(v.invs2send, inv)
			cnt++
		}
	}
	mutex.Unlock()
	return
}


type sortedkeys []uint64

func (sk sortedkeys) Len() int {
	return len(sk)
}

func (sk sortedkeys) Less(a, b int) bool {
	return sk[a]<sk[b]
}

func (sk sortedkeys) Swap(a, b int) {
	sk[a], sk[b] = sk[b], sk[a]
}


func net_stats(par string) {
	mutex.Lock()
	fmt.Printf("%d active net connections, %d outgoing\n", len(openCons), OutConsActive)
	srt := make(sortedkeys, len(openCons))
	cnt := 0
	for k, _ := range openCons {
		srt[cnt] = k
		cnt++
	}
	sort.Sort(srt)
	var tosnt, totrec uint64
	fmt.Print("                        Remote IP      LastCmd     Connected    LastActive")
	fmt.Print("    Received         Sent")
	//fmt.Print("    Version  UserAgent             Height   Addr Sent")
	fmt.Println()
	for idx := range srt {
		v := openCons[srt[idx]]
		fmt.Printf("%4d) ", idx+1)
		if v.writing {
			fmt.Print("w ")
		} else {
			fmt.Print("  ")
		}

		if v.listen {
			fmt.Print("<- ")
		} else {
			fmt.Print(" ->")
		}
		fmt.Printf(" %21s %12s", v.addr.Ip(), v.last_cmd)
		if v.connectedAt != 0 {
			now := time.Now().Unix()
			fmt.Printf("  %4d min ago", (now-v.connectedAt)/60)
			fmt.Printf("  %4d sec ago", now-int64(v.addr.Time))
			fmt.Print(bts2str(v.BytesReceived))
			fmt.Print(bts2str(v.BytesSent))
		}
		if v.node.version!=0 {
			fmt.Printf("  [%d * %s * %d]", v.node.version, v.node.agent, v.node.height)
		}

		if v.NextAddrSent != 0 {
			fmt.Printf("  %2d min ago", (uint32(time.Now().Unix())-(v.NextAddrSent-SendAddrsEvery))/60)
		}

		fmt.Println()

		totrec += v.BytesReceived
		tosnt += v.BytesSent
	}
	fmt.Printf("InvsSent:%d  BlockSent:%d  Received:%d MB, Sent %d MB\n", 
		InvsSent, BlockSent, totrec>>20, tosnt>>20)
	if *server && MyExternalAddr!=nil {
		fmt.Println("TCP server listening at external address", MyExternalAddr.String())
	}
	mutex.Unlock()
}


func net_drop(par string) {
	ip := net.ParseIP(par)
	if ip == nil || len(ip)!=16 {
		fmt.Println("Specify IP of the node to get disconnected")
		return
	}
	var ip4 [4]byte
	copy(ip4[:], ip[12:16])
	mutex.Lock()
	found := false
	for _, v := range openCons {
		if ip4==v.addr.Ip4 {
			v.broken = true
			found = true
			break
		}
	}
	mutex.Unlock()
	if found {
		fmt.Println("The connection is being dropped")
	} else {
		fmt.Println("You are not connected to such IP")
	}
}


func init() {
	newUi("net", false, net_stats, "Show network statistics")
	newUi("drop", false, net_drop, "Disconenct from node with a given IP")
}
