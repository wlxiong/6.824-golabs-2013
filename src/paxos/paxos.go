package paxos

//
// Paxos library, to be included in an application.
// Multiple applications will run, each including
// a Paxos peer.
//
// Manages a sequence of agreed-on values.
// The set of peers is fixed.
// Copes with network failures (partition, msg loss, &c).
// Does not store anything persistently, so cannot handle crash+restart.
//
// The application interface:
//
// px = paxos.Make(peers []string, me string)
// px.Start(seq int, v interface{}) -- start agreement on new instance
// px.Status(seq int) (decided bool, v interface{}) -- get info about an instance
// px.Done(seq int) -- ok to forget all instances <= seq
// px.Max() int -- highest instance seq known, or -1
// px.Min() int -- instances before this seq have been forgotten
//

import "net"
import "net/rpc"
import "log"
import "os"
import "syscall"
import "sync"
import "fmt"
import "math/rand"
import "time"

const (
  Unknown = "Unknown"
  Working = "Working"
  Decided = "Decided"
)

type Status string

type LogInstance struct {
  seq int
  // who sent the highest prepare
  who int
  // highest prepare seen
  np int
  // highest accept seen
  na int
  va interface{}
  status Status
}

type Paxos struct {
  mu sync.Mutex
  l net.Listener
  dead bool
  unreliable bool
  rpcCount int
  peers []string
  me int // index into peers[]

  // Your data here.
  logSeqs []int
  logInstances map[int]*LogInstance
  peerDoneSeqs map[int]int
  maxSeq int
  minSeq int
  doneSeq int
  maxPrepareSeen int
  maxPrepareOwner int
}

//
// call() sends an RPC to the rpcname handler on server srv
// with arguments args, waits for the reply, and leaves the
// reply in reply. the reply argument should be a pointer
// to a reply structure.
//
// the return value is true if the server responded, and false
// if call() was not able to contact the server. in particular,
// the replys contents are only valid if call() returned true.
//
// you should assume that call() will time out and return an
// error after a while if it does not get a reply from the server.
//
// please use call() to send all RPCs, in client.go and server.go.
// please do not change this function.
//
func call(srv string, name string, args interface{}, reply interface{}) bool {
  c, err := rpc.Dial("unix", srv)
  if err != nil {
    err1 := err.(*net.OpError)
    if err1.Err != syscall.ENOENT && err1.Err != syscall.ECONNREFUSED {
      // fmt.Printf("paxos Dial() failed: %v\n", err1)
    }
    return false
  }
  defer c.Close()
    
  err = c.Call(name, args, reply)
  if err == nil {
    return true
  }
  return false
}

func callWithRetry(srv string, name string, args interface{}, reply interface{}, maxRetries int) bool {
  for retry := 0; retry < maxRetries; retry++ {
    ok := call(srv, name, args, reply)
    if ok { return true }
  }
  return false
}

// hold px.mu before call this func
func (px *Paxos) UpdatePeerSeq(peer int, seq int, done int) {
  if px.maxSeq < seq { px.maxSeq = seq }

  px.peerDoneSeqs[peer] = done
  if len(px.peerDoneSeqs) < len(px.peers) {
    return
  }

  minDoneSeq := px.doneSeq
  for _, n := range px.peerDoneSeqs {
    if minDoneSeq > n { minDoneSeq = n }
  }

  if minDoneSeq + 1 > px.minSeq {
    log.Printf("[px][%d] update peer seq: new min seq %d, old min seq %d", px.me, minDoneSeq + 1, px.minSeq)
    px.minSeq = minDoneSeq + 1
    var activeSeqs []int
    log.Printf("[px][%d] update peer seq: num of logs %d", px.me, len(px.logSeqs))
    for _, s := range px.logSeqs {
      if s < px.minSeq {
        log.Printf("[px][%d] update peer seq: remove log with seq %d", px.me, s)
        delete(px.logInstances, s)
      } else {
        activeSeqs = append(activeSeqs, s)
      }
    }
    px.logSeqs = activeSeqs
  }
}

func (px *Paxos) HandleDecided(args *DecidedArgs, reply *DecidedReply) error {
  px.mu.Lock()
  defer px.mu.Unlock()

  px.UpdatePeerSeq(args.Peer, args.Seq, args.DoneSeq)

  if args.Seq < px.minSeq {
    log.Printf("[px][%d] ignore decided, seq %d < min seq %d", px.me, args.Seq, px.minSeq)
    reply.Err = Reject
    return nil
  }

  entry, ok := px.logInstances[args.Seq]
  if !ok {
    entry = &LogInstance{ args.Seq, -1, -1, -1, nil, Unknown }
    px.logInstances[args.Seq] = entry
    px.logSeqs = append(px.logSeqs, args.Seq)
  }

  // since the accepter unconditionally accept decided request
  // the local np can decrease and this accepter would accept
  // requests with higher number, but the decided value and
  // the status would not be changed
  if true || args.N >= entry.np {
    entry.seq = args.Seq
    entry.who = args.Peer
    entry.np = args.N
    entry.na = args.N
    entry.va = args.V
    entry.status = Decided
  }

  log.Printf("[px][%d] handle decided: args %+v entry %+v\n", px.me, args, entry)

  reply.Err = OK
  return nil
}

func (px *Paxos) HandlePrepare(args *PrepareArgs, reply *PrepareReply) error {
  px.mu.Lock()
  defer px.mu.Unlock()

  px.UpdatePeerSeq(args.Peer, args.Seq, args.DoneSeq)

  // acceptor's prepare(n) handler:
  // if n > n_p
  //   n_p = n
  //   reply prepare_ok(n_a, v_a)
  // else
  //   reply prepare_reject

  if args.Seq < px.minSeq {
    log.Printf("[px][%d] ignore prepare, seq %d < min seq %d", px.me, args.Seq, px.minSeq)
    reply.Err = Reject
    return nil
  }

  entry, ok := px.logInstances[args.Seq]
  if !ok {
    entry = &LogInstance{ args.Seq, -1, -1, -1, nil, Unknown }
    px.logInstances[args.Seq] = entry
    px.logSeqs = append(px.logSeqs, args.Seq)
  }

  log.Printf("[px][%d] handle prepare: args %+v entry %+v\n", px.me, args, entry)

  if args.N > entry.np {
    entry.who = args.Peer
    entry.np = args.N
    reply.Seq = args.Seq
    reply.Na = entry.na
    reply.Va = entry.va
    reply.Err = OK
  } else {
    reply.Who = entry.who
    reply.Np = entry.np
    reply.Err = Reject
  }

  return nil
}


func (px *Paxos) HandleAccept(args *AcceptArgs, reply *AcceptReply) error {
  px.mu.Lock()
  defer px.mu.Unlock()

  px.UpdatePeerSeq(args.Peer, args.Seq, args.DoneSeq)

  // acceptor's accept(n, v) handler:
  //   if n >= n_p
  //     n_p = n
  //     n_a = n
  //     v_a = v
  //     reply accept_ok(n)
  //   else
  //     reply accept_reject

  if args.Seq < px.minSeq {
    log.Printf("[px][%d] ignore accept, seq %d < min seq %d", px.me, args.Seq, px.minSeq)
    reply.Err = Reject
    return nil
  }

  entry, ok := px.logInstances[args.Seq]
  if !ok {
    entry = &LogInstance{ args.Seq, -1, -1, -1, nil, Unknown }
    px.logInstances[args.Seq] = entry
    px.logSeqs = append(px.logSeqs, args.Seq)
  }

  log.Printf("[px][%d] handle accept: args %+v entry %+v\n", px.me, args, entry)

  if args.N >= entry.np {
    entry.who = args.Peer
    entry.np = args.N
    entry.na = args.N
    entry.va = args.V
    reply.Seq = args.Seq
    reply.N = args.N
    reply.Err = OK
  } else {
    reply.Who = entry.who
    reply.Np = entry.np
    reply.Err = Reject
  }

  return nil
}

func (px *Paxos) FindLargerNumber(n int) int {
  num := px.me
  for num <= n {
    num += 10000
  }
  log.Printf("[px][%d] find larger prepare number: %d > %d", px.me, num, n)
  return num
}

const (
  ShortWait = 5
  LongWait = 100
)

func (px *Paxos) WaitForSomeMilliseconds(scale int) {
  numPeers := len(px.peers)
  sleepms := (rand.Intn(numPeers) + 1) * scale
  // sleepms := (len(px.peers) - px.me) * scale
  log.Printf("[px][%d] wait for %d ms...\n", px.me, sleepms)
  time.Sleep(time.Duration(sleepms) * time.Millisecond)
}

func (px *Paxos) DoPrepare(seq int, v interface{}) {
  px.mu.Lock()
  defer px.mu.Unlock()

  if seq < px.minSeq {
    log.Printf("[px][%d] ignore new proposal, seq %d < min seq %d", px.me, seq, px.minSeq)
    return
  }

  entry, ok := px.logInstances[seq]
  if ok {
    if entry.status == Working {
      log.Printf("[px][%d] working on seq %d", px.me, seq)
      return
    } else if entry.status == Decided {
      log.Printf("[px][%d] already decided seq %d", px.me, seq)
      return
    } else if entry.status == Unknown {
      log.Printf("[px][%d] re-propose uncommitted entry %+v, seq %d", px.me, entry, seq)
      entry.status = Working
    } else {
      panic(fmt.Sprintf("Unknown entry status %v", entry.status))
    }
  } else {
    log.Printf("[px][%d] add a new entry for %+v, seq %d", px.me, v, seq)
    px.logInstances[seq] = &LogInstance{ seq, -1, -1, -1, nil, Working }
    px.logSeqs = append(px.logSeqs, seq)
  }

  // proposer(v):
  // while not decided:
  //   choose n, unique and higher than any n seen so far
  //   send prepare(n) to all servers including self
  //   if prepare_ok(n_a, v_a) from majority:
  //     v' = v_a with highest n_a; choose own v otherwise
  //     send accept(n, v') to all
  //     if accept_ok(n) from majority:
  //       send decided(v') to all

  go func() {
    px.WaitForSomeMilliseconds(ShortWait)

    n := px.FindLargerNumber(px.me)
    px.maxPrepareSeen = n
    px.maxPrepareOwner = px.me

    numPeers := len(px.peers)
    numAccepted := 0
    numRejected := 0
    peerStatus := make(map[string]Err)
    maxPeerAcceptedNum := -1
    var acceptedVal interface {} = nil

    for !px.dead {
      log.Printf("[px][%d] start a new round, seq %d, n %d\n", px.me, seq, n)

      if seq < px.minSeq {
        log.Printf("[px][%d] stop current proposal, seq %d < min seq %d", px.me, seq, px.minSeq)
        return
      }

      func() {
        px.mu.Lock()
        defer px.mu.Unlock()
        entry, ok = px.logInstances[seq]
      }()

      if ok && entry.status == Decided {
        log.Printf("[px][%d] already decided, entry %+v\n", px.me, entry)
        return
      }

      args := PrepareArgs { seq, n, px.me, px.doneSeq }

      for pi, p := range px.peers {
        if px.dead { break }

        _, ok = peerStatus[p]
        if ok {
          continue
        }

        var reply PrepareReply
        ok := callWithRetry(p, "Paxos.HandlePrepare", &args, &reply, 5)
        if ok {
          peerStatus[p] = reply.Err
          if px.maxPrepareSeen < reply.Np {
            px.maxPrepareSeen = reply.Np
            px.maxPrepareOwner = reply.Who
          }
          if reply.Err == OK {
            numAccepted += 1
            if maxPeerAcceptedNum < reply.Na {
              maxPeerAcceptedNum = reply.Na
              acceptedVal = reply.Va
            }
          } else if reply.Err == Reject {
            numRejected += 1
          }
        } else {
          log.Printf("[px][%d] failed to call Paxos.HandlePrepare of peer %d", px.me, pi)
        }
      }

      log.Printf("[px][%d] prepare: seq %d n %d accept %d reject %d peers %d\n", px.me, seq, n, numAccepted, numRejected, numPeers)

      commitFailed := false
      if numAccepted > numPeers / 2 {
        // success
        if acceptedVal != nil {
          v = acceptedVal
        }
        ok := px.DoAccept(seq, v, n)
        if ok { break }
        commitFailed = true
      }

      // not decided yet
      px.WaitForSomeMilliseconds(ShortWait)
      if commitFailed || numRejected > 0 {
        // do a long wait if the competing proposer is greater than me
        if px.maxPrepareOwner > px.me {
          px.WaitForSomeMilliseconds(LongWait)
        }
        n = px.FindLargerNumber(px.maxPrepareSeen)
        numAccepted = 0
        numRejected = 0
        peerStatus = make(map[string]Err)
        maxPeerAcceptedNum = -1
        acceptedVal = nil
      }
    }

    px.DoNotify(seq, v, n)
  }()
}

func (px *Paxos) DoAccept(seq int, v interface{}, n int) bool {
  numPeers := len(px.peers)
  numAccepted := 0
  numRejected := 0
  peerStatus := make(map[string]Err)

  args := AcceptArgs { seq, n, v, px.me, px.doneSeq }
  for !px.dead {

    for pi, p := range px.peers {
      if px.dead { break }

      _, ok := peerStatus[p]
      if ok {
        continue
      }

      var reply AcceptReply
      ok = callWithRetry(p, "Paxos.HandleAccept", &args, &reply, 5)
      if ok {
        peerStatus[p] = reply.Err
        if px.maxPrepareSeen < reply.Np {
          px.maxPrepareSeen = reply.Np
          px.maxPrepareOwner = reply.Who
        }
        if reply.Err == OK {
          numAccepted += 1
        } else if reply.Err == Reject {
          numRejected += 1
        }
      } else {
        log.Printf("[px][%d] failed to call Paxos.HandleAccept of peer %d", px.me, pi)
      }
    }

    log.Printf("[px][%d] accept: seq %d n %d accept %d reject %d peers %d\n", px.me, seq, n, numAccepted, numRejected, numPeers)

    if numAccepted > numPeers / 2 {
      // success
      return true
    } else if numRejected > 0 {
      // failure
      return false
    }

    // not decided yet
    px.WaitForSomeMilliseconds(ShortWait)
  }

  return false
}

func (px *Paxos) DoNotify(seq int, v interface{}, n int) bool {
  // decided
  numPeers := len(px.peers)
  numNotified := 0
  peerStatus := make(map[string]Err)

  args := DecidedArgs { seq, n, v, px.me, px.doneSeq }
  for !px.dead {
    for pi, p := range px.peers {
      if px.dead { break }
      err, ok := peerStatus[p]
      if ok && err == OK { continue }
      var reply DecidedReply
      ok = callWithRetry(p, "Paxos.HandleDecided", &args, &reply, 5)
      if ok {
        numNotified += 1
        peerStatus[p] = reply.Err
      } else {
        log.Printf("[px][%d] failed to call Paxos.HandleDecided of peer %d", px.me, pi)
      }
    }

    if numNotified >= numPeers {
      break
    }

    // not all notified
    px.WaitForSomeMilliseconds(ShortWait)
  }

  return true
}

//
// the application wants paxos to start agreement on
// instance seq, with proposed value v.
// Start() returns right away; the application will
// call Status() to find out if/when agreement
// is reached.
//
func (px *Paxos) Start(seq int, v interface{}) {
  log.Printf("[px][%d] start: seq %d v %v\n", px.me, seq, v)
  px.DoPrepare(seq, v)
}

//
// the application on this machine is done with
// all instances <= seq.
//
// see the comments for Min() for more explanation.
//
func (px *Paxos) Done(seq int) {
  px.mu.Lock()
  defer px.mu.Unlock()
  log.Printf("[px][%d] done: seq %d\n", px.me, seq)
  if seq > px.doneSeq {
    px.doneSeq = seq
  }
}

//
// the application wants to know the
// highest instance sequence known to
// this peer.
//
func (px *Paxos) Max() int {
  px.mu.Lock()
  defer px.mu.Unlock()
  log.Printf("[px][%d] max: maxseq %d\n", px.me, px.maxSeq)
  return px.maxSeq
}

//
// Min() should return one more than the minimum among z_i,
// where z_i is the highest number ever passed
// to Done() on peer i. A peers z_i is -1 if it has
// never called Done().
//
// Paxos is required to have forgotten all information
// about any instances it knows that are < Min().
// The point is to free up memory in long-running
// Paxos-based servers.
//
// It is illegal to call Done(i) on a peer and
// then call Start(j) on that peer for any j <= i.
//
// Paxos peers need to exchange their highest Done()
// arguments in order to implement Min(). These
// exchanges can be piggybacked on ordinary Paxos
// agreement protocol messages, so it is OK if one
// peers Min does not reflect another Peers Done()
// until after the next instance is agreed to.
//
// The fact that Min() is defined as a minimum over
// *all* Paxos peers means that Min() cannot increase until
// all peers have been heard from. So if a peer is dead
// or unreachable, other peers Min()s will not increase
// even if all reachable peers call Done. The reason for
// this is that when the unreachable peer comes back to
// life, it will need to catch up on instances that it
// missed -- the other peers therefor cannot forget these
// instances.
// 
func (px *Paxos) Min() int {
  px.mu.Lock()
  defer px.mu.Unlock()
  log.Printf("[px][%d] min: minseq %d\n", px.me, px.minSeq)
  return px.minSeq
}

//
// the application wants to know whether this
// peer thinks an instance has been decided,
// and if so what the agreed value is. Status()
// should just inspect the local peer state;
// it should not contact other Paxos peers.
//
func (px *Paxos) Status(seq int) (bool, interface{}) {
  px.mu.Lock()
  defer px.mu.Unlock()

  if seq < px.minSeq {
    log.Printf("[px][%d] status: seq %d < min seq %d, ignored\n", px.me, seq, px.minSeq)
    // the instance has been decided but the log value was removed
    // the caller is expected to handle the null value
    return true, nil
  }

  entry, ok := px.logInstances[seq]
  if ok {
    log.Printf("[px][%d] status: seq %d, entry %+v\n", px.me, seq, entry)
    return entry.status == Decided, entry.va
  }

  return false, nil
}


//
// tell the peer to shut itself down.
// for testing.
// please do not change this function.
//
func (px *Paxos) Kill() {
  px.dead = true
  if px.l != nil {
    px.l.Close()
  }
}

//
// the application wants to create a paxos peer.
// the ports of all the paxos peers (including this one)
// are in peers[]. this servers port is peers[me].
//
func Make(peers []string, me int, rpcs *rpc.Server) *Paxos {
  px := &Paxos{}
  px.peers = peers
  px.me = me

  // Your initialization code here.
  px.logSeqs = make([]int, 0, 100)
  px.logInstances = make(map[int]*LogInstance)
  px.peerDoneSeqs = make(map[int]int)
  px.maxSeq = -1
  px.minSeq = 0
  px.doneSeq = -1

  log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)

  if rpcs != nil {
    // caller will create socket &c
    rpcs.Register(px)
  } else {
    rpcs = rpc.NewServer()
    rpcs.Register(px)

    // prepare to receive connections from clients.
    // change "unix" to "tcp" to use over a network.
    os.Remove(peers[me]) // only needed for "unix"
    l, e := net.Listen("unix", peers[me]);
    if e != nil {
      log.Fatal("listen error: ", e);
    }
    px.l = l
    
    // please do not change any of the following code,
    // or do anything to subvert it.
    
    // create a thread to accept RPC connections
    go func() {
      for px.dead == false {
        conn, err := px.l.Accept()
        if err == nil && px.dead == false {
          if px.unreliable && (rand.Int63() % 1000) < 100 {
            // discard the request.
            conn.Close()
          } else if px.unreliable && (rand.Int63() % 1000) < 200 {
            // process the request but force discard of reply.
            c1 := conn.(*net.UnixConn)
            f, _ := c1.File()
            err := syscall.Shutdown(int(f.Fd()), syscall.SHUT_WR)
            if err != nil {
              fmt.Printf("shutdown: %v\n", err)
            }
            px.rpcCount++
            go rpcs.ServeConn(conn)
          } else {
            px.rpcCount++
            go rpcs.ServeConn(conn)
          }
        } else if err == nil {
          conn.Close()
        }
        if err != nil && px.dead == false {
          fmt.Printf("Paxos(%v) accept: %v\n", me, err.Error())
        }
      }
    }()
  }


  return px
}
