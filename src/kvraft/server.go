package raftkv

import (
	"bytes"
	"labgob"
	"labrpc"
	"log"
	"raft"
	"sync"
)

const Debug = 0

func DPrintf(format string, a ...interface{}) (n int, err error) {
	if Debug > 0 {
		log.Printf(format, a...)
	}
	return
}

type Op struct {
	Key      string
	Value    string
	Op       string
	ClientId int64
	SeqId    int64
}

type LatestReply struct {
	SeqId int64    // Last request
	Reply GetReply // Last reply
}

type KVServer struct {
	mu      sync.Mutex
	me      int
	rf      *raft.Raft
	applyCh chan raft.ApplyMsg

	maxraftstate int // snapshot if log grows this big

	db            map[string]string
	notifyChs     map[int]chan struct{}
	persist       *raft.Persister
	snapshotIndex int
	shutdownCh    chan struct{}
	duplicate     map[int64]*LatestReply
}

func (kv *KVServer) Get(args *GetArgs, reply *GetReply) {
	if _, isLeader := kv.rf.GetState(); !isLeader {
		reply.WrongLeader = true
		reply.Err = ""
		return
	}

	DPrintf("[%d]: leader [%d] receive rpc: Get(%q).\n", kv.me, kv.me, args.Key)

	kv.mu.Lock()
	if dup, ok := kv.duplicate[args.ClientId]; ok {
		if args.SeqId <= dup.SeqId {
			kv.mu.Unlock()
			reply.WrongLeader = false
			reply.Err = OK
			reply.Value = dup.Reply.Value
			return
		}
	}

	cmd := Op{Key: args.Key, Op: "Get", ClientId: args.ClientId, SeqId: args.SeqId}
	index, term, _ := kv.rf.Start(cmd)
	ch := make(chan struct{})
	kv.notifyChs[index] = ch
	kv.mu.Unlock()

	reply.WrongLeader = false
	reply.Err = OK

	select {
	case <-ch:
		curTerm, isLeader := kv.rf.GetState()
		if !isLeader || term != curTerm {
			reply.WrongLeader = true
			reply.Err = ""
			return
		}

		kv.mu.Lock()
		if value, ok := kv.db[args.Key]; ok {
			reply.Value = value
		} else {
			reply.Err = ErrNoKey
		}
		kv.mu.Unlock()
	case <-kv.shutdownCh:
	}
}

func (kv *KVServer) PutAppend(args *PutAppendArgs, reply *PutAppendReply) {
	if _, isLeader := kv.rf.GetState(); !isLeader {
		reply.WrongLeader = true
		reply.Err = ""
		return
	}

	DPrintf("[%d]: leader [%d] receive rpc: PutAppend([%q] => (%q,%q), (%d-%d).\n", kv.me, kv.me,
		args.Op, args.Key, args.Value, args.ClientId, args.SeqId)

	kv.mu.Lock()
	if dup, ok := kv.duplicate[args.ClientId]; ok {
		if args.SeqId <= dup.SeqId {
			kv.mu.Unlock()
			reply.WrongLeader = false
			reply.Err = OK
			return
		}
	}

	cmd := Op{Key: args.Key, Value: args.Value, Op: args.Op, ClientId: args.ClientId, SeqId: args.SeqId}
	index, term, _ := kv.rf.Start(cmd)
	ch := make(chan struct{})
	kv.notifyChs[index] = ch
	kv.mu.Unlock()

	reply.WrongLeader = false
	reply.Err = OK

	select {
	case <-ch:
		curTerm, isLeader := kv.rf.GetState()
		if !isLeader || term != curTerm {
			reply.WrongLeader = true
			reply.Err = ""
			return
		}
	case <-kv.shutdownCh:
		return
	}
}

// applyDaemon receive applyMsg from Raft layer, apply to Key-Value state machine
// then notify related client if is leader
func (kv *KVServer) applyDaemon() {
	for {
		select {
		case <-kv.shutdownCh:
			DPrintf("[%d]: server [%d] is shutting down.\n", kv.me, kv.me)
			return
		case msg, ok := <-kv.applyCh:
			if ok {
				if msg.UseSnapshot {
					kv.mu.Lock()
					kv.readSnapshot(msg.Snapshot)
					kv.generateSnapshot(msg.CommandIndex)
					kv.mu.Unlock()
					continue
				}
				if msg.Command != nil && msg.CommandIndex > kv.snapshotIndex {
					cmd := msg.Command.(Op)
					kv.mu.Lock()
					if dup, ok := kv.duplicate[cmd.ClientId]; !ok || dup.SeqId < cmd.SeqId {
						switch cmd.Op {
						case "Get":
							kv.duplicate[cmd.ClientId] = &LatestReply{SeqId: cmd.SeqId,
								Reply: GetReply{Value: kv.db[cmd.Key]}}
						case "Put":
							kv.db[cmd.Key] = cmd.Value
							kv.duplicate[cmd.ClientId] = &LatestReply{SeqId: cmd.SeqId}
						case "Append":
							kv.db[cmd.Key] += cmd.Value
							kv.duplicate[cmd.ClientId] = &LatestReply{SeqId: cmd.SeqId}
						default:
							DPrintf("[%d]: server [%d] receive invalid cmd: [%v]\n", kv.me, kv.me, cmd)
							panic("invalid command operation")
						}
						if ok {
							DPrintf("[%d]: server [%d] apply index: [%d], cmd: [%v] (clientid: [%d], dup seqid: [%d] < [%d])\n",
								kv.me, kv.me, msg.CommandIndex, cmd, cmd.ClientId, dup.SeqId, cmd.SeqId)
						}
					}

					if needSnapshot(kv) {
						DPrintf("[%d]: server %d need generate snapshot @ %d (%d vs %d), client: %d.\n",
							kv.me, kv.me, msg.CommandIndex, kv.maxraftstate, kv.persist.RaftStateSize(), cmd.ClientId)
						kv.generateSnapshot(msg.CommandIndex)
					}

					if notifyCh, ok := kv.notifyChs[msg.CommandIndex]; ok && notifyCh != nil {
						close(notifyCh)
						delete(kv.notifyChs, msg.CommandIndex)
					}
					kv.mu.Unlock()
				}
			}
		}
	}
}

func needSnapshot(kv *KVServer) bool {
	if kv.maxraftstate < 0 {
		return false
	}
	if kv.maxraftstate < kv.persist.RaftStateSize() {
		return true
	}
	var abs = kv.maxraftstate - kv.persist.RaftStateSize()
	var threshold = kv.maxraftstate / 10
	if abs < threshold {
		return true
	}
	return false
}

func (kv *KVServer) generateSnapshot(index int) {
	w := new(bytes.Buffer)
	e := labgob.NewEncoder(w)

	kv.snapshotIndex = index

	e.Encode(kv.db)
	e.Encode(kv.snapshotIndex)
	e.Encode(kv.duplicate)

	data := w.Bytes()
	kv.rf.PersistAndSaveSnapshot(index, data)
}

func (kv *KVServer) readSnapshot(data []byte) {
	if data == nil || len(data) < 1 { // bootstrap without any state?
		return
	}
	r := bytes.NewBuffer(data)
	d := labgob.NewDecoder(r)

	kv.db = make(map[string]string)
	kv.duplicate = make(map[int64]*LatestReply)

	d.Decode(&kv.db)
	d.Decode(&kv.snapshotIndex)
	d.Decode(&kv.duplicate)
}

//
// the tester calls Kill() when a KVServer instance won't
// be needed again. you are not required to do anything
// in Kill(), but it might be convenient to (for example)
// turn off debug output from this instance.
//
func (kv *KVServer) Kill() {
	close(kv.shutdownCh)
	kv.rf.Kill()
}

//
// servers[] contains the ports of the set of
// servers that will cooperate via Raft to
// form the fault-tolerant key/value service.
// me is the index of the current server in servers[].
// the k/v server should store snapshots through the underlying Raft
// implementation, which should call persister.SaveStateAndSnapshot() to
// atomically save the Raft state along with the snapshot.
// the k/v server should snapshot when Raft's saved state exceeds maxraftstate bytes,
// in order to allow Raft to garbage-collect its log. if maxraftstate is -1,
// you don't need to snapshot.
// StartKVServer() must return quickly, so it should start goroutines
// for any long-running work.
//
func StartKVServer(servers []*labrpc.ClientEnd, me int, persister *raft.Persister, maxraftstate int) *KVServer {
	labgob.Register(Op{})

	kv := new(KVServer)
	kv.me = me
	kv.maxraftstate = maxraftstate

	kv.applyCh = make(chan raft.ApplyMsg)

	kv.db = make(map[string]string)
	kv.notifyChs = make(map[int]chan struct{})
	kv.persist = persister

	kv.shutdownCh = make(chan struct{})

	kv.duplicate = make(map[int64]*LatestReply)
	kv.readSnapshot(kv.persist.ReadSnapshot())
	kv.rf = raft.Make(servers, me, persister, kv.applyCh)
	go kv.applyDaemon()
	return kv
}
