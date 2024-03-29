package shardkv

import (
	"bytes"
	"labgob"
	"labrpc"
	"log"
	"raft"
	"shardmaster"
	"sync"
	"time"
)

const ShardMasterCheckInterval = 50 * time.Millisecond

const Debug = 1

func DPrintf(format string, a ...interface{}) (n int, err error) {
	if Debug > 0 {
		log.Printf(format, a...)
	}
	return
}

type CommandType int

const (
	Put CommandType = iota
	Append
	Get
	MigrateShard   // send shard and KV map to new owner
	ExportComplete // notify all replica of group exporting is completed
	ImportComplete // notify all replica of group importing is completed
)

type ShardStatus int

const (
	AVAILABLE ShardStatus = iota
	EXPORTING
	IMPORTING
	NOTOWNED
)

func (kv *ShardKV) shardStatusToString(s ShardStatus) string {
	switch s {
	case AVAILABLE:
		return "AVAILABLE"
	case EXPORTING:
		return "EXPORTING"
	case IMPORTING:
		return "IMPORTING"
	case NOTOWNED:
		return "NOTOWNED"
	}
	return "ERR_STATE"
}

type Op struct {
	// Your definitions here.
	// Field names must start with capital letters,
	// otherwise RPC will break.
	Method    string
	Key       string
	Value     string
	ClientId  int64
	SerialNum int64

	// used for migrate shards, followers need to catch up the latest changes
	Kvmap          map[string]string
	LatestRequests map[int64]int64

	// used for import and export
	ShardNumber         int
	BroadcastCfgVersion int
}

type ShardKV struct {
	mu           sync.Mutex
	me           int
	rf           *raft.Raft
	applyCh      chan raft.ApplyMsg
	make_end     func(string) *labrpc.ClientEnd
	gid          int
	masters      []*labrpc.ClientEnd
	maxraftstate int // snapshot if log grows this big

	mck *shardmaster.Clerk

	// Your definitions here.
	snapshotsEnabled bool
	isDecommissioned bool

	// Your definitions here.
	SnapshotIndex int

	Kvmap map[string]string

	// duplication detection table
	//Duplicate map[int64]int64
	latestRequests map[int]map[int64]int64 // Shard ID (int) -> Last request key-values (Client ID -> Request ID)

	requestHandlers map[int]chan raft.ApplyMsg

	ShardStatusList [shardmaster.NShards]ShardStatus

	LatestCfg shardmaster.Config

	shutdown chan struct{}
}

const AwaitLeaderCheckInterval = 10 * time.Millisecond

func (kv *ShardKV) Lock() {
	kv.mu.Lock()
}

func (kv *ShardKV) UnLock() {
	kv.mu.Unlock()
}

func (kv *ShardKV) snapshot(lastCommandIndex int) {

	kv.SnapshotIndex = lastCommandIndex
	w := new(bytes.Buffer)
	e := labgob.NewEncoder(w)
	e.Encode(kv.Kvmap)
	e.Encode(kv.SnapshotIndex)
	e.Encode(kv.latestRequests)
	e.Encode(kv.ShardStatusList)
	e.Encode(kv.LatestCfg)
	snapshot := w.Bytes()
	kv.rf.PersistAndSaveSnapshot(lastCommandIndex, snapshot)
}

func (kv *ShardKV) snapshotIfNeeded(lastCommandIndex int) {
	var threshold = int(0.95 * float64(kv.maxraftstate))
	if kv.maxraftstate != -1 && kv.rf.GetRaftStateSize() >= threshold {
		if kv.SnapshotIndex != lastCommandIndex {
			DPrintf("%d ShardKV server gid(%d) current snapshot index is %d, going to create snapshot for index %d. Current raft size is %d, threhold is %d",
				kv.me, kv.gid, kv.SnapshotIndex, lastCommandIndex, kv.rf.GetRaftStateSize(), threshold)
		}
		kv.snapshot(lastCommandIndex)
	}
}

func (kv *ShardKV) loadSnapshot(data []byte) {
	r := bytes.NewBuffer(data)
	d := labgob.NewDecoder(r)
	kvmap := make(map[string]string)
	duplicate := make(map[int]map[int64]int64)
	d.Decode(&kvmap)
	d.Decode(&kv.SnapshotIndex)
	d.Decode(&duplicate)
	d.Decode(&kv.ShardStatusList)
	d.Decode(&kv.LatestCfg)
	//DPrintf("%d load snapshot, snapshotIndex is %d, kvmap size is %d, duplciate map size is %d", kv.me, kv.SnapshotIndex, len(kvmap), len(duplicate))
	kv.Kvmap = kvmap
	kv.latestRequests = duplicate
}

// 我之前的做法是一个for loop 睡interval再继续， 每次loop all cached log看来的Index是否match存在
// 缺点是如果消息在Interval之内回来 我也继续睡。log会增长过大 没有效率.而且忘记测是否是leader！
// 现在的实现是一个general await api, 每隔Interval检查一下还是不是leader。 或者applied index committed消息抵达
// 如果是当初pass in的command, 直接trigger RPC handler
// 最后就是处理好正确或者错误的Case之后删除channel
func (kv *ShardKV) await(index int, term int, op Op) (success bool) {
	kv.Lock()
	awaitChan, ok := kv.requestHandlers[index]
	if !ok {
		awaitChan = make(chan raft.ApplyMsg, 1)
		kv.requestHandlers[index] = awaitChan
	} else {
		return true
	}

	kv.UnLock()

	for {
		select {
		case message := <-awaitChan:
			cmd := message.Command.(Op)

			kv.Lock()
			owns := kv.checkIfOwnsKey(cmd.Key)
			kv.UnLock()

			if index == message.CommandIndex && term == message.CommandTerm && owns {
				close(awaitChan)
				return true
			} else {
				// Message at index was not what we're expecting, must not be leader in majority partition
				return false
			}
		case <-time.After(800 * time.Millisecond):
			return false
		}
	}
}

func (kv *ShardKV) checkIfOwnsKey(key string) bool {
	// check if I owns the shard of the key
	if Debug == 1 {
		log.Println(kv.me, "Current shards map", kv.ShardStatusList)
	}

	shardNum := key2shard(key)
	if status := kv.ShardStatusList[shardNum]; status != AVAILABLE {
		DPrintf("%d DOES NOT OWN shard %d for key %s. Our gid is %d, status is %s",
			kv.me, shardNum, key, kv.gid, kv.shardStatusToString(status))
		return false
	} else {
		DPrintf("%d owns shard %d for key %s. Our gid is %d", kv.me, shardNum, key, kv.gid)
		return true
	}
}

func (kv *ShardKV) Get(args *GetArgs, reply *GetReply) {
	// Your code here.
	reply.WrongLeader = true
	reply.Err = ""
	if _, isLeader := kv.rf.GetState(); !isLeader {
		return
	}

	kv.Lock()

	if !kv.checkIfOwnsKey(args.Key) {
		kv.UnLock()
		reply.Err = ErrWrongGroup
		return
	}

	ops := Op{
		Method:    "Get",
		Key:       args.Key,
		ClientId:  args.ClientId,
		SerialNum: args.SerialNum,
	}
	kv.UnLock()

	index, term, isLeader := kv.rf.Start(ops)

	if !isLeader {
		reply.WrongLeader = true
		//log.Println(kv.me, "Gid", kv.gid, "Wrong leader1111!!!Get: my current kvmap", kv.Kvmap, "ops is", ops)
	} else {
		success := kv.await(index, term, ops)
		if !success {
			reply.WrongLeader = true
			//log.Println(kv.me, "Gid", kv.gid, "Wrong leader2222!!!Get: my current kvmap", kv.Kvmap, "ops is", ops)
		} else {
			kv.Lock()
			reply.WrongLeader = false

			if Debug == 1 {
				log.Println(kv.me, "my group", kv.gid, "Get: my current kvmap", kv.Kvmap, "ops is", ops)
			}
			if val, ok := kv.Kvmap[args.Key]; ok {
				reply.Value = val
				reply.Err = OK
			} else {
				reply.Err = ErrNoKey
			}
			kv.UnLock()
		}
	}
}

func (kv *ShardKV) PutAppend(args *PutAppendArgs, reply *PutAppendReply) {
	// Your code here.
	reply.WrongLeader = true
	reply.Err = ""
	if _, isLeader := kv.rf.GetState(); !isLeader {
		return
	}

	kv.Lock()

	if !kv.checkIfOwnsKey(args.Key) {
		kv.UnLock()
		reply.Err = ErrWrongGroup
		return
	}

	ops := Op{
		Method:    args.Op,
		Key:       args.Key,
		Value:     args.Value,
		ClientId:  args.ClientId,
		SerialNum: args.SerialNum,
	}
	kv.UnLock()

	DPrintf("%d my group %d append key %s value %s for shard %d", kv.me, kv.gid, args.Key, args.Value, key2shard(args.Key))
	index, term, isLeader := kv.rf.Start(ops)

	if !isLeader {
		reply.WrongLeader = true
	} else {
		success := kv.await(index, term, ops)
		if Debug == 1 {
			//log.Println(kv.me, "my group", kv.gid, "Put: my current kvmap", kv.Kvmap, "ops is", ops)
		}
		if !success {
			reply.WrongLeader = true
		} else {
			reply.WrongLeader = false
			reply.Err = OK
		}

	}
}

func (kv *ShardKV) isRequestDuplicate(shard int, clientId int64, requestId int64) bool {
	shardRequests, shardPresent := kv.latestRequests[shard]
	if shardPresent {
		lastRequest, isPresent := shardRequests[clientId]
		return isPresent && lastRequest == requestId
	}
	return false
}

func (kv *ShardKV) applyClientOp(cmd Op) {
	if !kv.isRequestDuplicate(key2shard(cmd.Key), cmd.ClientId, cmd.SerialNum) && cmd.Method != "Get" {
		// Double check that shard exists on this node, then write
		//if shardData, shardPresent := kv.data[key2shard(cmd.Key)]; shardPresent {
		if cmd.Method == "Put" {
			kv.Kvmap[cmd.Key] = cmd.Value
		} else if cmd.Method == "Append" {
			kv.Kvmap[cmd.Key] += cmd.Value
		}
		kv.latestRequests[key2shard(cmd.Key)][cmd.ClientId] = cmd.SerialNum // Safe since shard exists in `kv.data`
		//}
	}
}

func (kv *ShardKV) periodCheckApplyMsg() {
	for {
		select {
		case m, ok := <-kv.applyCh:
			if ok {
				kv.Lock()

				// ApplyMsg might be a request to load snapshot
				if m.UseSnapshot {
					kv.loadSnapshot(m.Snapshot)
					kv.UnLock()
					continue
				}

				cmd := m.Command.(Op)
				if m.CommandValid {
					// if we never process this client, or we never process this operation serial number
					// then we have a new request, we need to process it
					// Get request we do not care, handler will do the fetch.
					// For Put or Append, we do it here.
					//if dup, ok := kv.Duplicate[cmd.ClientId]; !ok || dup != cmd.SerialNum {
					// save the client id and its serial number
					switch cmd.Method {
					case "Put":
						kv.applyClientOp(cmd)
					case "Append":
						kv.applyClientOp(cmd)
					case "ExportComplete":
						{
							// remove kvmap, duplicate map, change ownership
							if kv.ShardStatusList[cmd.ShardNumber] == EXPORTING {
								DPrintf("%d export shard %d completed.", kv.me, cmd.ShardNumber)
								for k := range cmd.Kvmap {
									if cmd.ShardNumber == key2shard(k) {
										delete(kv.Kvmap, k)
									}
								}
								kv.ShardStatusList[cmd.ShardNumber] = NOTOWNED
							} else {
								DPrintf("%d err when receiving ExportComplete, Expected prev state: EXPORTING. but is %s", kv.me, kv.shardStatusToString(kv.ShardStatusList[cmd.ShardNumber]))
							}
						}
					case "ImportComplete":
						{
							// insert kvmap, duplicate map, change shard ownership to ourselves
							if kv.ShardStatusList[cmd.ShardNumber] == IMPORTING {
								DPrintf("%d import shard %d completed.", kv.me, cmd.ShardNumber)

								for k, v := range cmd.Kvmap {
									if cmd.ShardNumber == key2shard(k) {
										kv.Kvmap[k] = v
									}
								}
								// add the duplicate map
								for k, v := range cmd.LatestRequests {
									kv.latestRequests[cmd.ShardNumber][k] = v
								}

								kv.ShardStatusList[cmd.ShardNumber] = AVAILABLE
							} else {
								DPrintf("%d err when receiving ImportComplete, config version %d, Expected prev state: IMPORTING. but is %s", kv.me, kv.LatestCfg.Num, kv.shardStatusToString(kv.ShardStatusList[cmd.ShardNumber]))

								// hack
								for k, v := range cmd.Kvmap {
									if cmd.ShardNumber == key2shard(k) {
										kv.Kvmap[k] = v
									}
								}
								kv.ShardStatusList[cmd.ShardNumber] = AVAILABLE
							}
						}
					case "ApplyConfig":
						{
							kv.applyConfigOperation(cmd.BroadcastCfgVersion)
						}
					}
				}

				// Whenever key/value server detects that the Raft state size is approaching this threshold,
				// it should save a snapshot, and tell the Raft library that it has snapshotted,
				// so that Raft can discard old log entries.
				kv.snapshotIfNeeded(m.CommandIndex)

				// When we have applied message, we found the waiting channel(issued by RPC handler), forward the Ops
				if c, ok := kv.requestHandlers[m.CommandIndex]; ok {
					delete(kv.requestHandlers, m.CommandIndex)
					c <- m
				}
			}
			kv.UnLock()
		case <-kv.shutdown:
			return
		}

	}
}

//
// the tester calls Kill() when a RaftKV instance won't
// be needed again. you are not required to do anything
// in Kill(), but it might be convenient to (for example)
// turn off debug output from this instance.
//
func (kv *ShardKV) Kill() {
	//kv.Lock()
	//defer kv.UnLock()

	kv.rf.Kill()
	kv.isDecommissioned = true
	close(kv.shutdown)
}

/*
func (kv *ShardKV) applyConfigOperation(num int) {
	shardTransferInProgress := func() bool {
		for _, status := range kv.ShardStatusList {
			if status == EXPORTING || status == IMPORTING {
				return true
			}
		}
		return false
	}()

	if num == 1 {
		cfg := kv.mck.Query(num)
		newShardsToGroupMap := cfg.Shards
		// very fist valid config created in response to 1st Join RPC
		for shardIndex, newGid := range newShardsToGroupMap {
			if newGid == kv.gid {
				kv.ShardStatusList[shardIndex] = AVAILABLE
			} else {
				kv.ShardStatusList[shardIndex] = NOTOWNED
			}
		}
		kv.LatestCfg = cfg
		return
	}

	// We want to apply configurations in-order and when previous transfers are done
	if num == kv.LatestCfg.Num+1 && !shardTransferInProgress {
		cfg := kv.mck.Query(num)
		kv.LatestCfg = cfg
		for shard, gid := range cfg.Shards {
			shardStatus := kv.ShardStatusList[shard]
			if gid == kv.gid && shardStatus == NOTOWNED {
				DPrintf("%d gid %d, Configuration change: Now owner of shard %d. Change to Importing, Cfg number is %d", kv.me, kv.gid, shard, num)
				kv.ShardStatusList[shard] = IMPORTING

				// Query previous configurations until we find either there was a previous owner, or that we're the first owner
				lastConfig := cfg
				for lastConfig.Num > 1 && lastConfig.Shards[shard] == kv.gid {
					DPrintf("server %d, prev config number %d, preve config shard %d owner %d, we are group %d",
						kv.me, lastConfig.Num, shard, lastConfig.Shards[shard], kv.gid)
					lastConfig = kv.mck.Query(lastConfig.Num - 1)
				}

				DPrintf("server %d, after loop config number %d, preve config shard %d owner %d, we are group %d",
					kv.me, lastConfig.Num, shard, lastConfig.Shards[shard], kv.gid)
				if lastConfig.Num == 1 && lastConfig.Shards[shard] == kv.gid { // If this is the first config, and we're the owner
					//kvInfo("Creating new shard: %d", kv, shardNum)
					DPrintf("%d Creating new shard: %d", kv.me, shard)
					kv.ShardStatusList[shard] = AVAILABLE
				}
			} else if gid != kv.gid && shardStatus == AVAILABLE {
				kv.ShardStatusList[shard] = EXPORTING
				if kv.rf.IsLeader() {
					goneShard, destGid, cachedCfgNum := shard, gid, cfg.Num
					DPrintf("In Config num %d, server %d (our GID %d) sends shard %d to new GID %d", cachedCfgNum, kv.me,
						kv.gid, goneShard, destGid)

					destServers := make([]string, 0)
					for _,server := range cfg.Groups[destGid] {
						destServers = append(destServers, server)
					}
					kv.UnLock()
					kv.sendMigrateShard(goneShard, destGid, cachedCfgNum, destServers)
					kv.Lock()
				}
			} else if gid != kv.gid && shardStatus == IMPORTING {
				// We used to own the shard, and waiting for importing. But the new owner now is not us! Switch back to NOT_OWNED.
				DPrintf("%d (our GID %d) waiting on importing shard %d, but that shard NEVER transferred to us, new owner group %d",
					kv.me, kv.gid, shard, gid)
				kv.ShardStatusList[shard] = NOTOWNED
			}
		}
	}
}*/

func (kv *ShardKV) applyConfigOperation(num int) {
	shardTransferInProgress := func() bool {
		for _, status := range kv.ShardStatusList {
			if status == EXPORTING || status == IMPORTING {
				return true
			}
		}
		return false
	}()

	if kv.LatestCfg.Num+1 != num || shardTransferInProgress {
		return
	}

	cfg := kv.mck.Query(num)
	newShardsToGroupMap := cfg.Shards
	if cfg.Num > 1 {
		cachedPrevShardsToGroupMap := kv.LatestCfg.Shards
		// Update shards ownership
		for shardIndex, newGid := range newShardsToGroupMap {
			cachedGid := cachedPrevShardsToGroupMap[shardIndex]
			// if we owned, but lose, change to Exporting state
			shardStatus := kv.ShardStatusList[shardIndex]
			if cachedGid == kv.gid && newGid != kv.gid {
				if shardStatus == AVAILABLE {
					kv.ShardStatusList[shardIndex] = EXPORTING
					// Only leader sends migrate shard RPC, followers are waiting for confirmation once done
					if kv.rf.IsLeader() {
						goneShard, destGid, cachedCfgNum := shardIndex, newGid, cfg.Num
						DPrintf("In Config num%d, server %d (our GID %d) sends shard %d to new GID %d", cachedCfgNum, kv.me,
							kv.gid, goneShard, destGid)

						destServers := make([]string, 0)
						for _, server := range cfg.Groups[destGid] {
							destServers = append(destServers, server)
						}
						kv.UnLock()
						kv.sendMigrateShard(goneShard, destGid, cachedCfgNum, destServers)
						kv.Lock()
					}
				} else {
					DPrintf("%d (our GID %d) lost shard %d we owned, new gid %d, but our shard status is %s. Expected state 'AVAILABLE'",
						kv.me, kv.gid, shardIndex, newGid, kv.shardStatusToString(shardStatus))
					kv.ShardStatusList[shardIndex] = NOTOWNED //hack
				}
			} else if cachedGid != kv.gid && newGid == kv.gid {
				// if we gain new one, previous not owned, change to Importing state
				if shardStatus == NOTOWNED {
					DPrintf("%d (our GID %d) gain new shard %d from old gid %d, prev state 'NOTOWNED'",
						kv.me, kv.gid, shardIndex, cachedGid)
					if cachedGid != 0 {
						kv.ShardStatusList[shardIndex] = IMPORTING
					} else {
						kv.ShardStatusList[shardIndex] = AVAILABLE //hack
					}

					// Query previous configurations until we find either there was a previous owner, or that we're the first owner
					lastConfig := cfg
					for lastConfig.Num > 1 && lastConfig.Shards[shardIndex] == kv.gid {
						DPrintf("server %d, prev config number %d, preve config shard %d owner %d, we are group %d",
							kv.me, lastConfig.Num, shardIndex, lastConfig.Shards[shardIndex], kv.gid)
						lastConfig = kv.mck.Query(lastConfig.Num - 1)
					}

					DPrintf("server %d, after loop config number %d, preve config shard %d owner %d, we are group %d",
						kv.me, lastConfig.Num, shardIndex, lastConfig.Shards[shardIndex], kv.gid)
					if lastConfig.Num == 1 && lastConfig.Shards[shardIndex] == kv.gid { // If this is the first config, and we're the owner
						//kvInfo("Creating new shard: %d", kv, shardNum)
						DPrintf("%d Creating new shard: %d", kv.me, shardIndex)
						kv.ShardStatusList[shardIndex] = AVAILABLE
					}
				} else if newGid != kv.gid && shardStatus == IMPORTING {
					// We used to own the shard, and waiting for importing. But the new owner now is not us! Switch back to NOT_OWNED.
					DPrintf("%d (our GID %d) waiting on importing shard %d, but that shard NEVER transferred to us, new owner group %d",
						kv.me, kv.gid, shardIndex, newGid)
					kv.ShardStatusList[shardIndex] = NOTOWNED
				}
			}
		}
	} else if cfg.Num == 1 {
		// very fist valid config created in response to 1st Join RPC
		for shardIndex, newGid := range newShardsToGroupMap {
			if newGid == kv.gid {
				kv.ShardStatusList[shardIndex] = AVAILABLE
			} else {
				kv.ShardStatusList[shardIndex] = NOTOWNED
			}
		}
	}
	kv.LatestCfg = cfg
}

func (kv *ShardKV) periodCheckShardMasterConfig() {

	for {
		select {
		case <-time.After(ShardMasterCheckInterval):
			kv.Lock()
			if kv.isDecommissioned {
				kv.UnLock()
				return
			}

			// only leader fetch config and broadcast
			if !kv.rf.IsLeader() {
				kv.UnLock()
				continue
			}

			latestCfg := kv.mck.Query(-1)
			curCfgNum := kv.LatestCfg.Num + 1
			for ; curCfgNum <= latestCfg.Num; curCfgNum++ {
				var applyCfg shardmaster.Config
				if curCfgNum == latestCfg.Num {
					applyCfg = latestCfg
				} else {
					applyCfg = kv.mck.Query(curCfgNum)
				}

				ops := Op{
					Method:              "ApplyConfig",
					BroadcastCfgVersion: applyCfg.Num,
				}
				_, _, isleader := kv.rf.Start(ops)
				if !isleader {
					DPrintf("%d no long leader, should not apply", kv.me)
					break
				}
			}

			kv.UnLock()
		}
	}
}

//
// servers[] contains the ports of the servers in this group.
//
// me is the index of the current server in servers[].
//
// the k/v server should store snapshots through the underlying Raft
// implementation, which should call persister.SaveStateAndSnapshot() to
// atomically save the Raft state along with the snapshot.
//
// the k/v server should snapshot when Raft's saved state exceeds
// maxraftstate bytes, in order to allow Raft to garbage-collect its
// log. if maxraftstate is -1, you don't need to snapshot.
//
// gid is this group's GID, for interacting with the shardmaster.
//
// pass masters[] to shardmaster.MakeClerk() so you can send
// RPCs to the shardmaster.
//
// make_end(servername) turns a server name from a
// Config.Groups[gid][i] into a labrpc.ClientEnd on which you can
// send RPCs. You'll need this to send RPCs to other groups.
//
// look at client.go for examples of how to use masters[]
// and make_end() to send RPCs to the group owning a specific shard.
//
// StartServer() must return quickly, so it should start goroutines
// for any long-running work.
//
func StartServer(servers []*labrpc.ClientEnd, me int, persister *raft.Persister, maxraftstate int, gid int, masters []*labrpc.ClientEnd, make_end func(string) *labrpc.ClientEnd) *ShardKV {
	// call labgob.Register on structures you want
	// Go's RPC library to marshall/unmarshall.
	labgob.Register(Op{})
	labgob.Register(MigrateShardArgs{})
	labgob.Register(MigrateShardReply{})

	kv := new(ShardKV)
	kv.me = me
	kv.maxraftstate = maxraftstate
	kv.make_end = make_end
	kv.gid = gid
	kv.masters = masters

	// Your initialization code here.
	kv.snapshotsEnabled = (maxraftstate != -1)

	// Use something like this to talk to the shardmaster:
	kv.mck = shardmaster.MakeClerk(kv.masters)

	kv.applyCh = make(chan raft.ApplyMsg)
	kv.Kvmap = make(map[string]string)
	kv.latestRequests = make(map[int]map[int64]int64)

	for i := 0; i < shardmaster.NShards; i++ {
		kv.latestRequests[i] = make(map[int64]int64)
	}

	kv.isDecommissioned = false
	kv.shutdown = make(chan struct{})

	kv.requestHandlers = make(map[int]chan raft.ApplyMsg)

	if data := persister.ReadSnapshot(); kv.snapshotsEnabled && data != nil && len(data) > 0 {
		kv.loadSnapshot(data)
	}
	kv.rf = raft.Make(servers, me, persister, kv.applyCh)

	//log.Println("StartServer:", kv.me, "Gid", kv.gid, "current kvmap", kv.Kvmap, "LastSnapshot index", kv.SnapshotIndex)

	go kv.periodCheckShardMasterConfig()
	go kv.periodCheckApplyMsg()

	return kv
}

func (kv *ShardKV) startRequest(op Op, reply *RequestReply) {
	kv.Lock()
	index, term, isLeader := kv.rf.Start(op)
	kv.UnLock()

	if !isLeader {
		reply.WrongLeader = true
	} else {
		if success := kv.await(index, term, op); !success { // Request likely failed due to leadership change
			reply.WrongLeader = true
		}
	}
}

func (kv *ShardKV) MigrateShard(args *MigrateShardArgs, reply *MigrateShardReply) {
	kv.Lock()

	if !kv.rf.IsLeader() {
		reply.WrongLeader = true
		kv.UnLock()
		return
	}

	//if kv.LatestCfg.Num >= args.ConfigVersion {

	// if already handled, return
	if kv.ShardStatusList[args.ShardNumber] == AVAILABLE {
		kv.UnLock()
		return // dupliate request
	}

	DPrintf("MigrateShard Resp: In Config num %d, server %d (GID %d) got shard %d from another group", args.ConfigVersion, kv.me, kv.gid,
		args.ShardNumber)

	// Copy shard data
	data := make(map[string]string)
	for k := range args.Kvmap {
		if args.ShardNumber == key2shard(k) {
			data[k] = args.Kvmap[k]
		}
	}
	if Debug == 1 {
		log.Println(kv.me, "got kvmap", data)
	}

	// Copy shard's latest requests
	duplicateReqs := make(map[int64]int64)
	for k := range args.Duplicate {
		duplicateReqs[k] = args.Duplicate[k]
	}

	kv.UnLock()
	// start ops command to let followers know we are ready to take in new requests
	kv.broadcastMigrationStatus("ImportComplete", args.ShardNumber, args.ConfigVersion, data, duplicateReqs)
	reply.Err = OK
	//} else if args.ConfigVersion < kv.LatestCfg.Num {
	//	kv.UnLock()
	//	reply.Err = OK
	//}
}

func (kv *ShardKV) broadcastMigrationStatus(status string, shard int, cfgNum int,
	kvmap map[string]string, duplicates map[int64]int64) bool {
	kv.Lock()
	defer kv.UnLock()

	ops := Op{
		Method:              status,
		ShardNumber:         shard,
		BroadcastCfgVersion: cfgNum,
		Kvmap:               kvmap,
		LatestRequests:      duplicates,
	}

	_, _, isLeader := kv.rf.Start(ops)
	return isLeader
}

func (kv *ShardKV) sendMigrateShard(shard int, destGid int, cfgNum int, servers []string) {
	kv.Lock()
	req := MigrateShardArgs{
		ConfigVersion: cfgNum,
		ShardNumber:   shard,
		Kvmap:         make(map[string]string),
		Duplicate:     make(map[int64]int64),
	}

	// copy kvmap, and duplicate map
	for k, v := range kv.Kvmap {
		req.Kvmap[k] = v
	}
	for k, v := range kv.latestRequests[shard] {
		req.Duplicate[k] = v
	}

	kv.UnLock()

	DPrintf("%d sends shard %d to GID %d server", kv.me, shard, destGid)

	// for each server in gid, call it until found a leader and success
	for {
		for si := 0; si < len(servers); si++ {
			srv := kv.make_end(servers[si])
			var reply MigrateShardReply
			ok := srv.Call("ShardKV.MigrateShard", &req, &reply)
			if ok && reply.WrongLeader == false && reply.Err == OK {
				DPrintf("sendMigrateShard: %d sent shard %d to GID %d server. Got result ERR_OK. Broadcasting...",
					kv.me, shard, destGid)

				// Broadcast to all replica of my group that the migration for shard X is completed.
				kv.broadcastMigrationStatus("ExportComplete", shard, cfgNum, req.Kvmap, req.Duplicate)
				return
			}
			if ok && reply.Err == ErrWrongGroup {
				log.Println("Something is wrong, the group does not have the shards we thought")
				break
			}
		}
	}
}
