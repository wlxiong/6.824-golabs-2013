package lockservice

import "net/rpc"
import "time"
// import "fmt"

//
// the lockservice Clerk lives in the client
// and maintains a little state.
//
type Clerk struct {
	servers [2]string // primary port, backup port
	// Your definitions here.
	locks map[string]int64
}

func MakeClerk(primary string, backup string) *Clerk {
	ck := new(Clerk)
	ck.servers[0] = primary
	ck.servers[1] = backup
	// Your initialization code here.
	ck.locks = make(map[string]int64)
	return ck
}

//
// call() sends an RPC to the rpcname handler on server srv
// with arguments args, waits for the reply, and leaves the
// reply in reply. the reply argument should be the address
// of a reply structure.
//
// call() returns true if the server responded, and false
// if call() was not able to contact the server. in particular,
// reply's contents are valid if and only if call() returned true.
//
// you should assume that call() will time out and return an
// error after a while if it doesn't get a reply from the server.
//
// please use call() to send all RPCs, in client.go and server.go.
// please don't change this function.
//
func call(srv string, rpcname string,
	args interface{}, reply interface{}) bool {
	c, errx := rpc.Dial("unix", srv)
	if errx != nil {
		return false
	}
	defer c.Close()

	err := c.Call(rpcname, args, reply)
	if err == nil {
		return true
	}
	return false
}

//
// ask the lock service for a lock.
// returns true if the lock service
// granted the lock, false otherwise.
//
// you will have to modify this function.
//
func (ck *Clerk) Lock(lockname string) bool {
	// prepare the arguments.
	args := &LockArgs{}
	args.Lockname = lockname
	args.LockValue = time.Now().UnixNano()
	var reply LockReply

	// send an RPC request, wait for the reply.
  // fmt.Println("0 Lock", args)
	ok := call(ck.servers[0], "LockServer.Lock", args, &reply)
  // fmt.Println("0 Lock", args, ok, " reply ", reply.OK)
	if ok == true {
		ck.locks[args.Lockname] = args.LockValue
		return reply.OK
	} else {
		var reply1 LockReply
    // fmt.Println("1 Lock", args)
		ok = call(ck.servers[1], "LockServer.Lock", args, &reply1)
    // fmt.Println("1 Lock", args, ok, " reply ", reply1.OK)
		if ok == true {
			ck.locks[args.Lockname] = args.LockValue
			return reply1.OK
		}
	}

	return false
}

//
// ask the lock service to unlock a lock.
// returns true if the lock was previously held,
// false otherwise.
//

func (ck *Clerk) Unlock(lockname string) bool {
	args := &UnlockArgs{}
	args.Lockname = lockname
	args.LockValue = -time.Now().UnixNano()
	var reply UnlockReply

  // fmt.Println("0 Unlock", args)
	ok := call(ck.servers[0], "LockServer.Unlock", args, &reply)
  // fmt.Println("0 Unlock", args, ok, " reply ", reply.OK)
	if ok == true {
		ck.locks[args.Lockname] = args.LockValue
		return reply.OK
	} else {
		var reply1 LockReply
    // fmt.Println("1 Unlock", args)
		ok = call(ck.servers[1], "LockServer.Unlock", args, &reply1)
    // fmt.Println("1 Unlock", args, ok, " reply ", reply1.OK)
		if ok == true {
			ck.locks[args.Lockname] = args.LockValue
			return reply1.OK
		}
	}

	return false
}