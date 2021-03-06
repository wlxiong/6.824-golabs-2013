package lockservice

import "net"
import "net/rpc"
import "log"
import "sync"
import "fmt"
import "os"
import "io"
import "time"

type OpKey struct {
	Lockname string
	Lockid   int64
}

type LockServer struct {
	mu    sync.Mutex
	l     net.Listener
	dead  bool // for test_test.go
	dying bool // for test_test.go

	am_primary bool   // am I the primary?
	backup     string // backup's port

	// for each lock name, is it locked?
	locks      map[string]bool
	operations map[OpKey]bool
}

//
// server Lock RPC handler.
//
// you will have to modify this function
//
func (ls *LockServer) Lock(args *LockArgs, reply *LockReply) error {
	// Q: how to ensure backup sees operations in same order as primary?
	// A: Holding the lock until secondary replies ensures that all the
	//    requests are processed serially in the same order on primary
	//    and secondary. Of course the performance is not good.
	ls.mu.Lock()
	defer ls.mu.Unlock()

	// Locking granularity
	//   one mutex for whole lock_server?
	//   suppose we found handlers were often waiting for that one mutex
	//     what are reasonable options?
	//     one mutex per client?
	//     one mutex per lock?
	//   if one mutex per lock
	//     still need one mutex to protect table of locks
	//   danger of many locks---deadlock and races

	// Q: is it OK that client might send same request to both primary and backup?
	// Q: what happens if the primary fails just after forwarding to the backup?
	// A: If some requests are successfully processed on primary and secondary but the primary
	//    fails just before sending the replies to client, the client would re-issue the requests
	//    to the secondary. The secondary should store the results of all the operations.
	//    Only storing the final state is not enough, because the secondary might get requests that
	//    query the result of an old operation.

	// The above procedure is the at-most-once behavior:
	// Better RPC behavior: "at most once"
	// idea: server RPC code detects duplicate requests
	//   returns previous reply instead of re-running handler
	// client includes unique ID (UID) with each request
	//   uses same UID for re-send
	// server:
	//   if seen[uid]:
	// 	r = old[uid]
	//   else
	// 	r = handler()
	// 	old[uid] = r
	// 	seen[uid] = true

	// This trick also solves the following problem:
	// Q: is "at least once" easy for applications to cope with?
	//    Harder problem:
	//    Lock(a)
	//    Unlock(a) -- but network delays the packet
	//    Unlock(a) re-send, response arrives
	//    Lock(a)
	//    now network delivers the delayed Unlock(a) !!!

	//  TODO
	//  The first two ideas below can be implemented by an ordered dict.
	// 	some at-most-once complexities
	// 	how to ensure UID is unique?
	// 	big random number?
	// 	combine unique client ID (ip address?) with sequence #?
	// 	server must eventually discard info about old RPCs
	// 	when is discard safe?
	// 	idea:
	// 		unique client IDs
	// 		per-client RPC sequence numbers
	// 		client includes "seen all replies <= X" with every RPC
	// 		much like TCP sequence #s and acks
	// 	or only allow client one outstanding RPC at a time
	// 		arrival of seq+1 allows server to discard all <= seq
	// 	or client agrees to keep retrying for < 5 minutes
	// 		server discards after 5+ minutes
	// 	how to handle dup req while original is still executing?
	// 	server doesn't know reply yet; don't want to run twice
	// 	idea: "pending" flag per executing RPC; wait or ignore

	// What if an at-most-once server crashes?
	// 	if at-most-once duplicate info in memory, server will forget
	// 	and accept duplicate requests
	// 	maybe it should write the duplicate info to disk?
	// 	maybe replica server should also replicate duplicate info?

	// What about "exactly once"?
	// at-most-once plus unbounded retries

	key := OpKey{args.Lockname, args.Lockid}
	res, ok := ls.operations[key]
	if ok {
		reply.OK = res
		return nil
	}

	/*	fmt.Println(ls.locks)
		fmt.Println(ls.operations)*/
	locked := ls.locks[args.Lockname]
	if locked {
		reply.OK = false
	} else {
		reply.OK = true
		ls.locks[args.Lockname] = true
	}
	if ls.am_primary {
		var re LockReply
		ok := call(ls.backup, "LockServer.Lock", args, &re)
		if !ok {
			fmt.Println("Cannot call backup:", args)
		}
	}

	ls.operations[key] = reply.OK
	return nil
}

//
// server Unlock RPC handler.
//
func (ls *LockServer) Unlock(args *UnlockArgs, reply *UnlockReply) error {
	ls.mu.Lock()
	defer ls.mu.Unlock()

	key := OpKey{args.Lockname, args.Lockid}
	res, ok := ls.operations[key]
	if ok {
		reply.OK = res
		return nil
	}

	/*	fmt.Println(ls.locks)
		fmt.Println(ls.operations)*/
	locked := ls.locks[args.Lockname]
	if locked {
		reply.OK = true
		ls.locks[args.Lockname] = false
	} else {
		reply.OK = false
	}
	if ls.am_primary {
		var re LockReply
		ok := call(ls.backup, "LockServer.Unlock", args, &re)
		if !ok {
			fmt.Println("Cannot call backup:", args)
		}
	}

	ls.operations[key] = reply.OK
	return nil
}

//
// tell the server to shut itself down.
// for testing.
// please don't change this.
//
func (ls *LockServer) kill() {
	ls.dead = true
	ls.l.Close()
}

//
// hack to allow test_test.go to have primary process
// an RPC but not send a reply. can't use the shutdown()
// trick b/c that causes client to immediately get an
// error and send to backup before primary does.
// please don't change anything to do with DeafConn.
//
type DeafConn struct {
	c io.ReadWriteCloser
}

func (dc DeafConn) Write(p []byte) (n int, err error) {
	return len(p), nil
}
func (dc DeafConn) Close() error {
	return dc.c.Close()
}
func (dc DeafConn) Read(p []byte) (n int, err error) {
	return dc.c.Read(p)
}

func StartServer(primary string, backup string, am_primary bool) *LockServer {
	ls := new(LockServer)
	ls.backup = backup
	ls.am_primary = am_primary
	ls.locks = make(map[string]bool)
	ls.operations = make(map[OpKey]bool)

	// Your initialization code here.

	me := ""
	if am_primary {
		me = primary
	} else {
		me = backup
	}

	// tell net/rpc about our RPC server and handlers.
	rpcs := rpc.NewServer()
	rpcs.Register(ls)

	// prepare to receive connections from clients.
	// change "unix" to "tcp" to use over a network.
	os.Remove(me) // only needed for "unix"
	l, e := net.Listen("unix", me)
	if e != nil {
		log.Fatal("listen error: ", e)
	}
	ls.l = l

	// please don't change any of the following code,
	// or do anything to subvert it.

	// create a thread to accept RPC connections from clients.
	go func() {
		for ls.dead == false {
			conn, err := ls.l.Accept()
			if err == nil && ls.dead == false {
				if ls.dying {
					// process the request but force discard of reply.

					// without this the connection is never closed,
					// b/c ServeConn() is waiting for more requests.
					// test_test.go depends on this two seconds.
					go func() {
						time.Sleep(2 * time.Second)
						conn.Close()
					}()
					ls.l.Close()

					// this object has the type ServeConn expects,
					// but discards writes (i.e. discards the RPC reply).
					deaf_conn := DeafConn{c: conn}

					rpcs.ServeConn(deaf_conn)

					ls.dead = true
				} else {
					go rpcs.ServeConn(conn)
				}
			} else if err == nil {
				conn.Close()
			}
			if err != nil && ls.dead == false {
				fmt.Printf("LockServer(%v) accept: %v\n", me, err.Error())
				ls.kill()
			}
		}
	}()

	return ls
}
