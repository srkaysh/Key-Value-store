package raftkv

import (
	"crypto/rand"
	"labrpc"
	"math/big"
	"time"
)

var clients = make(map[int64]bool)

type Clerk struct {
	servers []*labrpc.ClientEnd

	leader   int64
	seqid    int64
	clientid int64
}

func nrand() int64 {
	max := big.NewInt(int64(1) << 62)
	bigx, _ := rand.Int(rand.Reader, max)
	x := bigx.Int64()
	return x
}

func generateId() int64 {
	for {
		x := nrand()
		if clients[x] {
			continue
		}
		clients[x] = true
		return x
	}
}

func MakeClerk(servers []*labrpc.ClientEnd) *Clerk {
	ck := new(Clerk)
	ck.servers = servers

	ck.leader = int64(len(servers))
	ck.seqid = 1
	ck.clientid = generateId()
	DPrintf("Clerk: [%d]", ck.clientid)

	return ck
}

//
// fetch the current value for a key.
// returns "" if the key does not exist.
// keeps trying forever in the face of all other errors.
//
// you can send an RPC with code like this:
// ok := ck.servers[i].Call("KVServer.Get", &args, &reply)
//
// the types of args and reply (including whether they are pointers)
// must match the declared types of the RPC handler function's
// arguments. and reply must be passed as a pointer.
//
func (ck *Clerk) Get(key string) string {
	DPrintf("Clerk: Get: [%s]\n", key)
	count := len(ck.servers)
	for {
		args := &GetArgs{Key: key, ClientId: ck.clientid, SeqId: ck.seqid}
		reply := new(GetReply)

		ck.leader %= int64(count)
		done := make(chan bool, 1)
		go func() {
			ok := ck.servers[ck.leader].Call("KVServer.Get", args, reply)
			done <- ok
		}()
		select {
		case <-time.After(200 * time.Millisecond): // rpc timeout: 200ms
			ck.leader++
			continue
		case ok := <-done:
			if ok && !reply.WrongLeader {
				ck.seqid++
				if reply.Err == OK {
					return reply.Value
				}
				return ""
			}
			ck.leader++
		}
	}
	return ""
}

//
// shared by Put and Append.
//
// you can send an RPC with code like this:
// ok := ck.servers[i].Call("KVServer.PutAppend", &args, &reply)
//
// the types of args and reply (including whether they are pointers)
// must match the declared types of the RPC handler function's
// arguments. and reply must be passed as a pointer.
//
func (ck *Clerk) PutAppend(key string, value string, op string) {
	DPrintf("Clerk: PutAppend: [%q] => (%q,%q) from: [%d]\n", op, key, value, ck.clientid)
	count := len(ck.servers)
	for {
		args := &PutAppendArgs{Key: key, Value: value, Op: op, ClientId: ck.clientid, SeqId: ck.seqid}
		reply := new(PutAppendReply)

		ck.leader %= int64(count)
		done := make(chan bool, 1)
		go func() {
			ok := ck.servers[ck.leader].Call("KVServer.PutAppend", args, reply)
			done <- ok
		}()
		select {
		case <-time.After(200 * time.Millisecond): // rpc timeout: 200ms
			ck.leader++
			continue
		case ok := <-done:
			if ok && !reply.WrongLeader && reply.Err == OK {
				ck.seqid++
				return
			}
			ck.leader++
		}
	}
}

func (ck *Clerk) Put(key string, value string) {
	ck.PutAppend(key, value, "Put")
}
func (ck *Clerk) Append(key string, value string) {
	ck.PutAppend(key, value, "Append")
}
