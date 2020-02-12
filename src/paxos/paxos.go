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

type LogInstance struct {
    // highest prepare seen
    np int
    // highest accept seen
    na int
    va interface{}
    decided bool
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
  logIds []int
  logInstances map[int]*LogInstance
  maxSeq int
  minSeq int
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
      fmt.Printf("paxos Dial() failed: %v\n", err1)
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

func (px *Paxos) HandleDecided(args *DecidedArgs, reply *DecidedReply) error {
  px.mu.Lock()
  defer px.mu.Unlock()
  if px.maxSeq < args.Seq { px.maxSeq = args.Seq }

  entry, ok := px.logInstances[args.Seq]
  if !ok {
    entry = &LogInstance{ args.N, args.N, args.V, true }
    px.logInstances[args.Seq] = entry
  } else {
    entry.decided = true
  }

  reply.Err = OK
  return nil
}

func (px *Paxos) HandlePrepare(args *PrepareArgs, reply *PrepareReply) error {
  px.mu.Lock()
  defer px.mu.Unlock()
  if px.maxSeq < args.Seq { px.maxSeq = args.Seq }

  // acceptor's prepare(n) handler:
  // if n > n_p
  //   n_p = n
  //   reply prepare_ok(n_a, v_a)
  // else
  //   reply prepare_reject

  entry, ok := px.logInstances[args.Seq]
  if !ok {
    entry = &LogInstance{ -1, -1, nil, false }
    px.logInstances[args.Seq] = entry
  }

  // log.Printf("handle prepare: me %d seq %d arg.N %d", px.me, args.Seq, args.N)
  // log.Printf("handle prepare: seq %d np %d na %d decided %t", args.Seq, entry.np, entry.na, entry.decided)

  if args.N > entry.np {
    entry.np = args.N
    reply.Seq = args.Seq
    reply.Na = entry.na
    reply.Va = entry.va
    reply.Err = OK
  } else {
    reply.Na = entry.na
    reply.Err = Reject
  }

  // log.Printf("handle prepare: seq %d np %d na %d decided %t", args.Seq, entry.np, entry.na, entry.decided)

  return nil
}


func (px *Paxos) HandleAccept(args *AcceptArgs, reply *AcceptReply) error {
  px.mu.Lock()
  defer px.mu.Unlock()
  if px.maxSeq < args.Seq { px.maxSeq = args.Seq }

  // acceptor's accept(n, v) handler:
  //   if n >= n_p
  //     n_p = n
  //     n_a = n
  //     v_a = v
  //     reply accept_ok(n)
  //   else
  //     reply accept_reject

  entry, ok := px.logInstances[args.Seq]
  if !ok {
    entry = &LogInstance{ -1, -1, nil, false }
    px.logInstances[args.Seq] = entry
  }

  // log.Printf("handle accept: me %d seq %d arg.N %d", px.me, args.Seq, args.N)
  // log.Printf("handle accept: seq %d np %d na %d decided %t", args.Seq, entry.np, entry.na, entry.decided)

  if args.N >= entry.np {
    entry.np = args.N
    entry.na = args.N
    entry.va = args.V
    reply.Seq = args.Seq
    reply.N = args.N
    reply.Err = OK
  } else {
    reply.N = args.N
    reply.Err = Reject
  }

  // log.Printf("handle accept: seq %d np %d na %d decided %t", args.Seq, entry.np, entry.na, entry.decided)

  return nil
}

func (px *Paxos) FindLargerNumber(n int) int {
  num := px.me
  for num <= n {
    num += 10000
  }
  return num
}

func (px *Paxos) DoPrepare(seq int, v interface{}) {
  px.mu.Lock()
  defer px.mu.Unlock()

  _, ok := px.logInstances[seq]
  if ok {
    return
  }

  px.logIds = append(px.logIds, seq)
  px.logInstances[seq] = &LogInstance{ -1, -1, nil, false }

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
    decided := false
    n := px.FindLargerNumber(px.me)

    numPeers := len(px.peers)
    numAccepted := 0
    numRejected := 0
    peerStatus := make(map[string]Err)
    maxPeerAcceptedNum := -1
    maxPeerNum := n
    var acceptedVal interface {} = nil
    // acceptProcStart := false

    for !px.dead && !decided {
      args := PrepareArgs { Seq: seq, N: n }

      for _, p := range px.peers {
        _, ok = peerStatus[p]
        if ok {
          continue
        }

        var reply PrepareReply
        ok := call(p, "Paxos.HandlePrepare", &args, &reply)
        if ok {
          peerStatus[p] = reply.Err
          if maxPeerNum < reply.Na {
            maxPeerNum = reply.Na
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
        }
      }

      fmt.Printf("prepare: n %d accept %d reject %d\n", n, numAccepted, numRejected)

      if numAccepted > numPeers / 2 {
        // success
        if acceptedVal != nil {
          v = acceptedVal
        }
        decided = px.DoAccept(seq, v, n)
      } else if numRejected > numPeers / 2 {
        // failure
        n = px.FindLargerNumber(maxPeerNum)
        numAccepted = 0
        numRejected = 0
        peerStatus = make(map[string]Err)
        maxPeerAcceptedNum = -1
        maxPeerNum = -1
        acceptedVal = nil
      }
    }

    // decided
    numAcked := 0
    peerStatus = make(map[string]Err)
    args := DecidedArgs { seq, n, v }
    for !px.dead && numAcked < numPeers {
      for _, p := range px.peers {
        err, ok := peerStatus[p]
        if ok && err == OK { continue }
        var reply DecidedReply
        ok = call(p, "Paxos.HandleDecided", &args, &reply)
        if ok {
          numAcked += 1
          peerStatus[p] = reply.Err
        }
      }
    }

  }()
}

func (px *Paxos) DoAccept(seq int, v interface{}, n int) bool {
  numPeers := len(px.peers)
  numAccepted := 0
  numRejected := 0
  peerStatus := make(map[string]Err)

  args := AcceptArgs { Seq: seq, N: n, V: v }
  for !px.dead {

    for _, p := range px.peers {
      _, ok := peerStatus[p]
      if ok {
        continue
      }

      var reply AcceptReply
      ok = call(p, "Paxos.HandleAccept", &args, &reply)
      if ok {
        peerStatus[p] = reply.Err
        if reply.Err == OK {
          numAccepted += 1
        } else if reply.Err == Reject {
          numRejected += 1
        }
      }
    }

    fmt.Printf("accept: n %d accept %d reject %d\n", n, numAccepted, numRejected)

    if numAccepted > numPeers / 2 {
      // success
      px.mu.Lock()
      defer px.mu.Unlock()
      entry, _ := px.logInstances[seq]
      entry.decided = true
      entry.va = v
      return true
    } else if numRejected > numPeers / 2 {
      // failure
      return false
    }
  }

  return false
}

//
// the application wants paxos to start agreement on
// instance seq, with proposed value v.
// Start() returns right away; the application will
// call Status() to find out if/when agreement
// is reached.
//
func (px *Paxos) Start(seq int, v interface{}) {
  px.DoPrepare(seq, v)
}

//
// the application on this machine is done with
// all instances <= seq.
//
// see the comments for Min() for more explanation.
//
func (px *Paxos) Done(seq int) {
  // Your code here.
}

//
// the application wants to know the
// highest instance sequence known to
// this peer.
//
func (px *Paxos) Max() int {
  px.mu.Lock()
  defer px.mu.Unlock()
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
  // You code here.
  return 0
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
  entry, ok := px.logInstances[seq]
  if ok {
    fmt.Printf("status, me %d entry %v\n", px.me, entry)
    return entry.decided, entry.va
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
  px.logIds = make([]int, 0, 100)
  px.logInstances = make(map[int]*LogInstance)
  px.maxSeq = -1

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
