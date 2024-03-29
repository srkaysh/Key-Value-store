package raft

//
// this is an outline of the API that raft must expose to
// the service (or tester). see comments below for
// each of these functions for more details.
//
// rf = Make(...)
//   create a new Raft server.
// rf.Start(command interface{}) (index, term, isleader)
//   start agreement on a new log entry
// rf.GetState() (term, isLeader)
//   ask a Raft for its current term, and whether it thinks it is leader
// ApplyMsg
//   each time a new entry is committed to the log, each Raft peer
//   should send an ApplyMsg to the service (or tester)
//   in the same server.
//

import (
	"bytes"
	"labgob"
	"labrpc"
	"log"
	"math/rand"
	"sync"
	"time"
)

const HeartBeat = 90 * time.Millisecond
const CommitApplyIdleCheck = 15 * time.Millisecond
const LeaderPeerTick = 5 * time.Millisecond

//
// as each Raft peer becomes aware that successive log entries are
// committed, the peer should send an ApplyMsg to the service (or
// tester) on the same server, via the applyCh passed to Make(). set
// CommandValid to true to indicate that the ApplyMsg contains a newly
// committed log entry.
//
// in Lab 3 you'll want to send other kinds of messages (e.g.,
// snapshots) on the applyCh; at that point you can add fields to
// ApplyMsg, but set CommandValid to false for these other uses.
//
type ApplyMsg struct {
	CommandValid bool
	Command      interface{}
	CommandIndex int
	CommandTerm  int
	UseSnapshot  bool
	Snapshot     []byte
}

type ServerState string

const (
	Follower  ServerState = "Follower"
	Candidate             = "Candidate"
	Leader                = "Leader"
)

type Log struct {
	Cmd   interface{}
	Term  int
	Index int
}

//
// A Go object implementing a single Raft peer.
//
type Raft struct {
	mu        sync.Mutex          // Lock to protect shared access to this peer's state
	peers     []*labrpc.ClientEnd // RPC end points of all peers
	persister *Persister          // Object to hold this peer's persisted state
	me        int                 // this peer's index into peers[]

	// Your data here (2A, 2B, 2C).
	// Look at the paper's Figure 2 for a description of what
	// state a Raft server must maintain.
	leaderID int
	term     int
	votedFor int

	lastHeartBeat time.Time
	state         ServerState
	timeout       time.Duration

	commitIndex int
	lastApplied int

	log        []Log
	nextIndex  []int
	matchIndex []int

	isDecommissioned bool

	sendAppendChan []chan struct{}

	//snapshot states
	lastSnapshotIndex int
	lastSnapshotTerm  int

	shutdown      chan struct{} // shutdown gracefully
	notifyApplyCh chan struct{} // notify to apply

	applyChan chan ApplyMsg
}

func (rf *Raft) debug(format string, a ...interface{}) {
	//args := append([]interface{}{rf.me, rf.term, rf.state}, a...)
	//log.Printf("[INFO] Raft:[Id:%d|Term:%d|State:%s|] " + format, args...)
}

func (rf *Raft) Me() int {
	return rf.me
}

func (rf *Raft) Lock() {
	rf.mu.Lock()
}

func (rf *Raft) UnLock() {
	rf.mu.Unlock()
}

func (rf *Raft) isLeader() bool {
	return rf.state == Leader
}

func (rf *Raft) IsLeader() bool {
	return rf.state == Leader
}

func (rf *Raft) turnToFollow() {
	rf.state = Follower
	rf.votedFor = -1
	rf.persist()
}

// return currentTerm and whether this server
// believes it is the leader.
func (rf *Raft) GetState() (int, bool) {
	rf.Lock()
	term := rf.term
	isLeader := rf.isLeader()
	rf.UnLock()

	return term, isLeader
}

func (rf *Raft) getPersistState() []byte {
	w := new(bytes.Buffer)
	e := labgob.NewEncoder(w)

	e.Encode(rf.term)
	e.Encode(rf.votedFor)
	e.Encode(rf.log)
	e.Encode(rf.lastSnapshotIndex)
	e.Encode(rf.lastSnapshotTerm)

	data := w.Bytes()
	return data
}

// because snapshot will replace committed log entries in log
// thus the length of rf.log is different from(less than or equal) rf.logIndex
func (rf *Raft) getOffsetIndex(i int) int {
	return i - rf.lastSnapshotIndex
}

func (rf *Raft) PersistAndSaveSnapshot(lastIncludedIndex int, snapshot []byte) {
	rf.Lock()
	defer rf.UnLock()
	if lastIncludedIndex > rf.lastSnapshotIndex {
		if i, isPresent := rf.findLogIndex(lastIncludedIndex); isPresent {
			entry := rf.log[i]
			rf.lastSnapshotIndex = entry.Index
			rf.lastSnapshotTerm = entry.Term

			// rf.log = rf.log[i+1:]
			// let's try make last log index at 0
			rf.log = rf.log[i:]

			data := rf.getPersistState()
			rf.persister.SaveStateAndSnapshot(data, snapshot)
		}

		/*		truncationStartIndex := rf.getOffsetIndex(lastIncludedIndex)
				rf.lastSnapshotIndex = lastIncludedIndex
				rf.lastSnapshotTerm = rf.log[truncationStartIndex].Term
				rf.log = append([]Log{}, rf.log[truncationStartIndex:]...) // log entry previous at lastIncludedIndex at 0 now
		*/
	}
}

func (rf *Raft) LoadSnapShot() []byte {
	return rf.persister.ReadSnapshot()
}

func (rf *Raft) GetRaftStateSize() int {
	return rf.persister.RaftStateSize()
}

//
// save Raft's persistent state to stable storage,
// where it can later be retrieved after a crash and restart.
// see paper's Figure 2 for a description of what should be persistent.
//
func (rf *Raft) persist() {
	// Your code here (2C).

	w := new(bytes.Buffer)
	e := labgob.NewEncoder(w)
	e.Encode(rf.term)
	e.Encode(rf.votedFor)
	e.Encode(rf.log)
	e.Encode(rf.lastSnapshotIndex)
	e.Encode(rf.lastSnapshotTerm)

	data := w.Bytes()
	rf.persister.SaveRaftState(data)
}

//
// restore previously persisted state.
//
func (rf *Raft) readPersist(data []byte) {
	if data == nil || len(data) < 1 { // bootstrap without any state?
		return
	}

	r := bytes.NewBuffer(data)
	d := labgob.NewDecoder(r)
	currentTerm := 0
	votedFor := -1
	logs := []Log{}
	lastSnapshotIndex := 0
	lastSnapshotTerm := 0

	if d.Decode(&currentTerm) != nil ||
		d.Decode(&votedFor) != nil ||
		d.Decode(&logs) != nil ||
		d.Decode(&lastSnapshotIndex) != nil ||
		d.Decode(&lastSnapshotTerm) != nil {
		log.Println("Something bad is happening in decoder!")
	} else {
		//rf.Lock()
		rf.term = currentTerm
		rf.votedFor = votedFor
		rf.log = logs
		rf.lastSnapshotIndex = lastSnapshotIndex
		rf.lastSnapshotTerm = lastSnapshotTerm
		//rf.UnLock()
	}
	rf.persist()
}

//
// example RequestVote RPC arguments structure.
// field names must start with capital letters!
//
type RequestVoteArgs struct {
	// Your data here (2A, 2B).
	Term         int
	CandidateId  int
	LastLogIndex int
	LastLogTerm  int
}

//
// example RequestVote RPC reply structure.
// field names must start with capital letters!
//
type RequestVoteReply struct {
	// Your data here (2A).
	Term        int
	VoteGranted bool
}

func (rf *Raft) checkIfLogUpdateToDate(lastLogIndex int, lastLogTerm int) bool {
	lastI, lastT := rf.getLastLogEntry()
	if lastT == lastLogTerm {
		return lastI <= lastLogIndex
	} else {
		return lastT < lastLogTerm
	}
}

//
// example RequestVote RPC handler.
//
func (rf *Raft) RequestVote(args *RequestVoteArgs, reply *RequestVoteReply) {
	// Your code here (2A, 2B).

	rf.Lock()
	defer rf.UnLock()

	reply.Term = rf.term
	// check if log is up-to-date
	updateToDate := rf.checkIfLogUpdateToDate(args.LastLogIndex, args.LastLogTerm)

	if args.Term > rf.term {
		rf.turnToFollow()
		rf.term = args.Term
	}

	if args.Term < rf.term {
		reply.VoteGranted = false
	} else if (rf.votedFor == -1 || args.CandidateId == rf.votedFor) && updateToDate {
		rf.votedFor = args.CandidateId
		reply.VoteGranted = true
		// restart election timer if we voted someone. This caused unreliable test random fail
		rf.lastHeartBeat = time.Now()
	}
	DPrintf("%d is requested vote for %d. His term %d, last log index %d, last log term %d, but our term %d, lastsnapshot index %d, lastsnapshot term %d, is log updated? %t. did I vote? %t",
		rf.me, args.CandidateId, args.Term, args.LastLogIndex, args.LastLogTerm, rf.term, rf.lastSnapshotIndex, rf.lastSnapshotTerm, updateToDate, reply.VoteGranted)

	rf.persist()
}

type AppendEntriesArgs struct {
	Term        int
	LeaderId    int
	PreLogIndex int
	PreLogTerm  int

	// entries
	Entries      []Log
	LeaderCommit int
}

type AppendEntriesReply struct {
	Term                int
	Success             bool
	ConflictingLogTerm  int
	ConflictingLogIndex int
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (rf *Raft) AppendEntries(args *AppendEntriesArgs, reply *AppendEntriesReply) {
	// Your code here (2A, 2B).
	rf.Lock()
	defer rf.UnLock()

	reply.Term = rf.term

	if args.Term < rf.term {
		reply.Success = false
		return
	}

	if args.Term >= rf.term {
		rf.turnToFollow()
		rf.term = args.Term
		rf.leaderID = args.LeaderId
		// IMPORTANT!!!
		// After turn myself to follower, I also reset voteFor to NULL, this caused
		// I may revote to someone else in the same term! (TOOK ME 3 DAYS DEBUGGING)
		// The fix is below, set votedFor to leader in each AppendEntries handler.
		rf.votedFor = args.LeaderId
	}

	if rf.leaderID == args.LeaderId {
		rf.lastHeartBeat = time.Now()
	}

	// Critical code: If the leader's matchIndex[us] < our lastsnapshot index, we send leader our lastSnapshot + 1
	// so leader will track nextIndex[us] = lastSnapshot + 1, this fixed the infinity loop
	if args.PreLogIndex < rf.lastSnapshotIndex {
		reply.Success = false
		reply.ConflictingLogIndex = rf.lastSnapshotIndex + 1
		return
	}

	// find args.PreLogIndex position in current log
	// Note: preIndex is -1 in two cases:
	// 1. No match. Reply false!
	// 2. args.prev is the beginning of log, we need to process
	preLogIndexInCurLogPos := -1
	for i, v := range rf.log {
		if v.Index == args.PreLogIndex {
			if v.Term == args.PreLogTerm {
				preLogIndexInCurLogPos = i
				break
			} else {
				// now we have the conflict term
				reply.ConflictingLogTerm = v.Term
			}
		}
	}

	PrevIsBeginningOfLog := args.PreLogIndex == 0 && args.PreLogTerm == 0
	PrevMatchedSnapshotIndexAndTerm := (args.PreLogIndex == rf.lastSnapshotIndex) && (args.PreLogTerm == rf.lastSnapshotTerm)
	// compare start from preIndex + 1
	// if we found the preIndex in current log, or prev is the beginning, or we found the previous in snapshot, we continue!
	// else, we give what we have (the end of our current log)
	if preLogIndexInCurLogPos >= 0 || PrevIsBeginningOfLog || PrevMatchedSnapshotIndexAndTerm {
		entryIndex := 0
		for i := preLogIndexInCurLogPos + 1; i < len(rf.log); i++ {
			entryConsistent := func() bool {
				localEntry, argsEntry := rf.log[i], args.Entries[entryIndex]
				return (localEntry.Term == argsEntry.Term) && (localEntry.Index == argsEntry.Index)
			}

			if entryIndex >= len(args.Entries) || !entryConsistent() {
				// delete extra inconsistent entries, exclude the current i since it is inconsistent
				rf.log = rf.log[:i]
				break
			} else {
				entryIndex++
			}
		}

		// append the rest of NEW entries, starting from entryIndex
		if entryIndex < len(args.Entries) {
			rf.log = append(rf.log, args.Entries[entryIndex:]...)
		}

		// Update the commit index
		oldCommitIndex := rf.commitIndex
		if args.LeaderCommit > rf.commitIndex {
			var latestLogIndex = rf.lastSnapshotIndex
			if len(rf.log) > 0 {
				latestLogIndex = rf.log[len(rf.log)-1].Index
			}

			if args.LeaderCommit < latestLogIndex {
				rf.commitIndex = args.LeaderCommit
			} else {
				rf.commitIndex = latestLogIndex
			}
		}
		if rf.commitIndex > oldCommitIndex {
			rf.notifyApplyCh <- struct{}{}
		}

		reply.Success = true
	} else {
		if reply.ConflictingLogTerm == 0 && len(rf.log) > 0 {
			reply.ConflictingLogTerm = rf.log[len(rf.log)-1].Term
		}

		for _, v := range rf.log {
			// get the conflict index for the conflict term
			if v.Term == reply.ConflictingLogTerm {
				reply.ConflictingLogIndex = v.Index
				break
			}
		}
		reply.Success = false
	}
	DPrintf("%d receives entries from leader %d, his term %d, prevLogIndex %d, prevLogTerm %d, leaderCommint %d, but our term %d, lastsnapshotIndex %d, lastsnapshotTerm %d our commitIndex %d. our reply conflict index is %d. Reply success? %t",
		rf.me, args.LeaderId, args.Term, args.PreLogIndex, args.PreLogTerm, args.LeaderCommit, rf.term, rf.lastSnapshotIndex, rf.lastSnapshotTerm, rf.commitIndex, reply.ConflictingLogIndex, reply.Success)
	rf.persist()
}

//
// example code to send a RequestVote RPC to a server.
// server is the index of the target server in rf.peers[].
// expects RPC arguments in args.
// fills in *reply with RPC reply, so caller should
// pass &reply.
// the types of the args and reply passed to Call() must be
// the same as the types of the arguments declared in the
// handler function (including whether they are pointers).
//
// The labrpc package simulates a lossy network, in which servers
// may be unreachable, and in which requests and replies may be lost.
// Call() sends a request and waits for a reply. If a reply arrives
// within a timeout interval, Call() returns true; otherwise
// Call() returns false. Thus Call() may not return for a while.
// A false return can be caused by a dead server, a live server that
// can't be reached, a lost request, or a lost reply.
//
// Call() is guaranteed to return (perhaps after a delay) *except* if the
// handler function on the server side does not return.  Thus there
// is no need to implement your own timeouts around Call().
//
// look at the comments in ../labrpc/labrpc.go for more details.
//
// if you're having trouble getting RPC to work, check that you've
// capitalized all field names in structs passed over RPC, and
// that the caller passes the address of the reply struct with &, not
// the struct itself.
//
func (rf *Raft) sendRequestVote(server int, args *RequestVoteArgs, reply *RequestVoteReply) bool {
	ok := rf.peers[server].Call("Raft.RequestVote", args, reply)
	return ok
}

func (rf *Raft) appendEntries(server int, args AppendEntriesArgs, reply *AppendEntriesReply) bool {
	ok := rf.peers[server].Call("Raft.AppendEntries", &args, reply)
	return ok
}

func (rf *Raft) installSnapshot(server int, args InstallSnapshotArgs, reply *InstallSnapshotReply) bool {
	ok := rf.peers[server].Call("Raft.InstallSnapshot", &args, reply)
	return ok
}

func (rf *Raft) getLogSize() int {
	size := len(rf.log)
	return size
}

func (rf *Raft) getLastLogIndex() int {

	if len(rf.log) == 0 {
		return rf.lastSnapshotIndex
	}

	latestLogIndex := rf.log[len(rf.log)-1].Index
	return latestLogIndex
}

//
// the service using Raft (e.g. a k/v server) wants to start
// agreement on the next command to be appended to Raft's log. if this
// server isn't the leader, returns false. otherwise start the
// agreement and return immediately. there is no guarantee that this
// command will ever be committed to the Raft log, since the leader
// may fail or lose an election. even if the Raft instance has been killed,
// this function should return gracefully.
//
// the first return value is the index that the command will appear at
// if it's ever committed. the second return value is the current
// term. the third return value is true if this server believes it is
// the leader.
//
func (rf *Raft) Start(command interface{}) (int, int, bool) {
	// Your code here (2B).
	rf.Lock()
	defer rf.UnLock()

	if rf.isLeader() == false {
		return -1, -1, false
	}

	nextIndex := func() int {
		if len(rf.log) > 0 {
			return rf.log[len(rf.log)-1].Index + 1
		}
		return Max(1, rf.lastSnapshotIndex+1)
	}()

	entry := Log{
		Cmd:   command,
		Term:  rf.term,
		Index: nextIndex,
	}
	rf.log = append(rf.log, entry)

	rf.persist()

	//rf.debug("add command %v", command, entry)
	return entry.Index, rf.term, true
}

//
// the tester calls Kill() when a Raft instance won't
// be needed again. you are not required to do anything
// in Kill(), but it might be convenient to (for example)
// turn off debug output from this instance.
//
func (rf *Raft) Kill() {
	// Your code here, if desired.
	rf.Lock()
	defer rf.UnLock()
	rf.isDecommissioned = true
	close(rf.shutdown)
}

func (rf *Raft) updateCommitIndex() {
	i := rf.getLogSize()
	for i > 0 {
		v := rf.log[i-1]
		count := 1
		//log.Println(v, "i is", i, "commit index", rf.commitIndex, "every body matches", rf.matchIndex, "v term", v.Term, "my cur term", rf.term)

		// a leader cannot determine commitment using log entries from older terms
		if v.Term == rf.term && v.Index > rf.commitIndex {
			// check if has majority
			// Note: this j the value, not index
			for serverIndex, j := range rf.matchIndex {
				if serverIndex == rf.me {
					continue
				}
				// for any matchIndex value (j), if larger or equal to i (the log index starting from last), incre count.
				if j >= v.Index {
					count++
				}
			}
		}
		if count > len(rf.peers)/2 {
			rf.commitIndex = v.Index
			//log.Println(rf.me, "peer got commit index", rf.commitIndex, "count is", count, "peer num is", len(rf.peers)/2)
			rf.notifyApplyCh <- struct{}{}
			break
		}
		i--
	}
}

func Min(a, b int) int {
	if a < b {
		return a
	} else {
		return b
	}
}

func (rf *Raft) sendAppendEntries(s int, sendAppendChan chan struct{}) {
	rf.Lock()
	if !rf.isLeader() || rf.isDecommissioned {
		rf.UnLock()
		return
	}

	entries := []Log{}
	preLogIndex := 0
	preLogTerm := 0
	lastLogIndex, _ := rf.getLastLogEntry()

	if rf.nextIndex[s] <= rf.lastSnapshotIndex {
		DPrintf("%d going to send snapshot to instance %d, nextindex is %d, snapshot index is %d", rf.me, s, rf.nextIndex[s], rf.lastSnapshotIndex)
		rf.UnLock()
		go rf.sendSnapshot(s, sendAppendChan)
		return
	}

	// if we have log entry, and our logs contain nextIndex[s]
	if lastLogIndex > 0 && lastLogIndex >= rf.nextIndex[s] {
		// send all missing entries!
		for i, v := range rf.log {
			// current i plus 1 is the log index. Only proceed if we are currently at next index for peer
			if v.Index == rf.nextIndex[s] {
				if i > 0 {
					// get previous index's log index.
					entr := rf.log[i-1]
					preLogIndex = entr.Index
					preLogTerm = entr.Term
				} else {
					preLogIndex = rf.lastSnapshotIndex
					preLogTerm = rf.lastSnapshotTerm
				}
				// Note: length of log minus previous log index is len(rf.log)-i
				// the current one we need to send, so from current to the end
				entries = make([]Log, len(rf.log)-i)
				copy(entries, rf.log[i:])
				//log.Println(rf.me, "mama sending entries", entries, "to", s, "my log", rf.log, "pre index", preLogIndex)
				break
			}
		}
	} else {
		if len(rf.log) > 0 {
			// nextIndex larger than our last log index, we send heartbeat since we assume the peer is up-to-date
			// if it is not, then they will reply false back to us, so we decrement
			preLogIndex, preLogTerm = rf.getLastLogEntry()
			//log.Println(rf.me, "last log index term are", preLogIndex, preLogTerm)
		} else {
			preLogIndex = rf.lastSnapshotIndex
			preLogTerm = rf.lastSnapshotTerm
		}
	}

	args := AppendEntriesArgs{
		Term:         rf.term,
		LeaderId:     rf.me,
		PreLogIndex:  preLogIndex,
		PreLogTerm:   preLogTerm,
		Entries:      entries,
		LeaderCommit: rf.commitIndex,
	}
	//log.Println(rf.me, "send append entries args to peer", s, "args are:", args, "last log index term are", preLogIndex, preLogTerm)

	rf.UnLock()

	reply := &AppendEntriesReply{}

	request := func() bool {
		return rf.appendEntries(s, args, reply)
	}

	ok := SendRPCRequest(request)

	rf.Lock()
	defer rf.UnLock()

	if ok {
		if rf.state != Leader || rf.isDecommissioned || args.Term != rf.term {
			return
		}

		if reply.Term > rf.term {
			rf.term = reply.Term
			rf.turnToFollow()
			rf.persist()
			return // we are out
		}

		if reply.Success {
			// update log
			if len(entries) > 0 {
				rf.matchIndex[s] = preLogIndex + len(entries)
				rf.nextIndex[s] = rf.matchIndex[s] + 1
				rf.updateCommitIndex()
			}
		} else {
			// go back to conflict log index minus one, so we are safe. but the lowest index is 1.
			/*max := reply.ConflictingLogIndex-1
			if max < 1 {
				max = 1
			}
			rf.nextIndex[s] = max*/
			lastIndex := rf.getLastLogIndex()
			rf.nextIndex[s] = Max(1, Min(reply.ConflictingLogIndex, lastIndex))
			sendAppendChan <- struct{}{} // Signals to leader-peer process that appends need to occur
		}
	}
}

func (rf *Raft) appendEntriesLoopForPeer(server int, sendAppendChan chan struct{}) {
	if rf.isDecommissioned {
		return
	}
	ticker := time.NewTicker(LeaderPeerTick)

	go rf.sendAppendEntries(server, sendAppendChan)
	lastEntrySent := time.Now()

	for {
		rf.Lock()
		if rf.state != Leader || rf.isDecommissioned {
			ticker.Stop()
			rf.UnLock()
			break
		}
		rf.UnLock()

		select {
		case <-sendAppendChan: // Signal that we should send a new append to this peer
			lastEntrySent = time.Now()
			go rf.sendAppendEntries(server, sendAppendChan)
		case currentTime := <-ticker.C:
			if currentTime.Sub(lastEntrySent) >= HeartBeat {
				lastEntrySent = time.Now()
				// we fire and contine, otherwise the loop stuck if the peer no responding
				go rf.sendAppendEntries(server, sendAppendChan)
			}
		}
	}
}

func (rf *Raft) becomeLeader() {
	rf.Lock()
	defer rf.UnLock()

	rf.state = Leader
	rf.leaderID = rf.me
	rf.debug("I am a leader!")

	rf.nextIndex = make([]int, len(rf.peers))
	rf.matchIndex = make([]int, len(rf.peers))
	rf.sendAppendChan = make([]chan struct{}, len(rf.peers))

	for p := range rf.peers {
		if p == rf.me {
			continue
		}

		rf.nextIndex[p] = rf.getLastLogIndex() + 1 // Should be initialized to leader's last log index + 1
		rf.matchIndex[p] = 0                       // Index of highest log entry known to be replicated on server

		rf.sendAppendChan[p] = make(chan struct{}, 1000)

		go rf.appendEntriesLoopForPeer(p, rf.sendAppendChan[p])
	}
}

func (rf *Raft) getLastLogEntry() (int, int) {
	if len(rf.log) > 0 {
		entry := rf.log[len(rf.log)-1]
		return entry.Index, entry.Term
	}
	return rf.lastSnapshotIndex, rf.lastSnapshotTerm
}

func (rf *Raft) beginElection() {
	rf.Lock()
	rf.state = Candidate
	rf.term++
	rf.votedFor = rf.me

	voteCount := 1
	cachedTerm := rf.term

	index, term := rf.getLastLogEntry()
	req := &RequestVoteArgs{
		Term:         cachedTerm,
		CandidateId:  rf.me,
		LastLogIndex: index,
		LastLogTerm:  term,
	}

	rf.persist()
	rf.UnLock()

	for s := range rf.peers {
		if s == rf.me {
			continue
		}

		go func(serverIndex int) {
			reply := &RequestVoteReply{}
			//log.Println(rf.me, "hi vote for me", req)

			request := func() bool {
				return rf.sendRequestVote(serverIndex, req, reply)
			}

			ok := SendRPCRequest(request)
			if ok {
				rf.Lock()
				cachedCurTerm := rf.term
				cachedCurState := rf.state
				rf.UnLock()

				if reply.Term > cachedCurTerm {
					rf.Lock()
					rf.term = reply.Term
					rf.turnToFollow()
					rf.persist()
					rf.UnLock()
				} else if cachedTerm == cachedCurTerm { // only process in same term
					if reply.VoteGranted {
						rf.Lock()
						voteCount++
						curVote := voteCount
						rf.UnLock()

						if curVote > len(rf.peers)/2 && cachedCurState == Candidate {
							rf.Lock()
							rf.state = Leader
							rf.UnLock()
							go rf.becomeLeader()
						}
					}
				}
			}
		}(s)

	}
}

func (rf *Raft) startElectionProcess() {
	electionTimeout := func() time.Duration { // Randomized timeouts between [500, 800)-ms
		return (500 + time.Duration(rand.Intn(300))) * time.Millisecond
	}

	rf.timeout = electionTimeout()
	currentTime := <-time.After(rf.timeout)

	rf.Lock()
	defer rf.UnLock()

	if !rf.isDecommissioned {
		// Start election process if we're not a leader and the haven't received a heartbeat for `electionTimeout`
		if rf.state != Leader && currentTime.Sub(rf.lastHeartBeat) >= rf.timeout {
			go rf.beginElection()
		}
		go rf.startElectionProcess()
	}
}

func Max(x, y int) int {
	if x > y {
		return x
	}
	return y
}

func (rf *Raft) findLogIndex(logIndex int) (int, bool) {
	for i, e := range rf.log {
		if e.Index == logIndex {
			return i, true
		}
	}
	return -1, false
}

func (rf *Raft) Replay() {
	rf.Lock()
	cachedLocalApplied := rf.lastApplied
	cachedSnapshotIndex := rf.lastSnapshotIndex
	cachedCommitIndex := rf.commitIndex
	rf.UnLock()

	if cachedLocalApplied < cachedSnapshotIndex {
		rf.applyChan <- ApplyMsg{
			UseSnapshot:  true,
			Snapshot:     rf.persister.ReadSnapshot(),
			CommandValid: false,
		}
		rf.Lock()
		rf.lastApplied = cachedSnapshotIndex
		rf.UnLock()
	} else if cachedCommitIndex > cachedLocalApplied {
		rf.Lock()
		startIndex, _ := rf.findLogIndex(rf.lastApplied + 1)
		startIndex = Max(startIndex, 0) // If start index wasn't found, it's because it's a part of a snapshot

		endIndex := -1
		for i := startIndex; i < len(rf.log); i++ {
			if rf.log[i].Index <= rf.commitIndex {
				endIndex = i
			}
		}

		if endIndex >= 0 { // We have some entries to locally commit
			entries := make([]Log, endIndex-startIndex+1)
			copy(entries, rf.log[startIndex:endIndex+1])
			rf.UnLock()

			// Hold no locks so that slow local applies don't deadlock the system
			for _, v := range entries {
				rf.applyChan <- ApplyMsg{
					UseSnapshot:  false,
					CommandIndex: v.Index,
					Command:      v.Cmd,
					CommandValid: true,
				}
			}
			rf.Lock()
			rf.lastApplied += len(entries)
		}
		rf.UnLock()
	}
}

func (rf *Raft) startLocalApplyProcess(applyChan chan ApplyMsg) {
	for {
		select {
		case <-rf.notifyApplyCh:

			rf.Lock()
			cachedCommitIndex := rf.commitIndex
			cachedLocalApplied := rf.lastApplied
			cachedSnapshotIndex := rf.lastSnapshotIndex
			rf.UnLock()

			if cachedLocalApplied < cachedSnapshotIndex {
				DPrintf("%d needs to load snapshot. My commit index is %d, lastApplied is %d, lastsnapshot index is %d, lastsnapshot term is %d",
					rf.me, rf.commitIndex, rf.lastApplied, rf.lastSnapshotIndex, rf.lastSnapshotTerm)
				applyChan <- ApplyMsg{
					UseSnapshot:  true,
					Snapshot:     rf.persister.ReadSnapshot(),
					CommandValid: false,
				}

				rf.Lock()
				rf.lastApplied = cachedSnapshotIndex
				rf.persist()
				rf.UnLock()

			} else if cachedCommitIndex > cachedLocalApplied {
				rf.Lock()
				startIndex, _ := rf.findLogIndex(rf.lastApplied + 1)
				startIndex = Max(startIndex, 0) // If start index wasn't found, it's because it's a part of a snapshot

				endIndex := -1
				for i := startIndex; i < len(rf.log); i++ {
					if rf.log[i].Index <= rf.commitIndex {
						endIndex = i
					}
				}

				if endIndex >= 0 { // We have some entries to locally commit
					entries := make([]Log, endIndex-startIndex+1)
					copy(entries, rf.log[startIndex:endIndex+1])
					rf.UnLock()

					// Hold no locks so that slow local applies don't deadlock the system
					for _, v := range entries {
						applyChan <- ApplyMsg{
							UseSnapshot:  false,
							CommandIndex: v.Index,
							CommandTerm:  v.Term,
							Command:      v.Cmd,
							CommandValid: true,
						}
					}
					rf.Lock()
					rf.lastApplied += len(entries)
				}
				rf.UnLock()
			}
		case <-rf.shutdown:
			return
		}
	}
}

//
// the service or tester wants to create a Raft server. the ports
// of all the Raft servers (including this one) are in peers[]. this
// server's port is peers[me]. all the servers' peers[] arrays
// have the same order. persister is a place for this server to
// save its persistent state, and also initially holds the most
// recent saved state, if any. applyCh is a channel on which the
// save its persistent state, and also initially holds the most
// recent saved state, if any. applyCh is a channel on which the
// tester or service expects Raft to send ApplyMsg messages.
// Make() must return quickly, so it should start goroutines
// for any long-running work.
//
func Make(peers []*labrpc.ClientEnd, me int,
	persister *Persister, applyCh chan ApplyMsg) *Raft {
	rf := &Raft{}
	rf.peers = peers
	rf.persister = persister
	rf.me = me

	// Your initialization code here (2A, 2B, 2C).
	rf.state = Follower
	rf.term = 0
	rf.votedFor = -1
	rf.commitIndex = 0
	rf.lastApplied = 0
	rf.log = []Log{}

	rf.lastSnapshotIndex = 0
	rf.lastSnapshotTerm = 0

	rf.notifyApplyCh = make(chan struct{}, 10000)
	rf.shutdown = make(chan struct{})
	rf.applyChan = make(chan ApplyMsg)
	rf.applyChan = applyCh

	// initialize from state persisted before a crash
	rf.readPersist(persister.ReadRaftState())

	go rf.startElectionProcess()
	go rf.startLocalApplyProcess(applyCh)

	return rf
}

// InstallSnapshot RPC
type InstallSnapshotArgs struct {
	Term              int
	LeaderId          int
	LastIncludedIndex int
	LastIncludedTerm  int
	Data              []byte
}

type InstallSnapshotReply struct {
	Term int
}

func (rf *Raft) sendSnapshot(peerIndex int, sendAppendChan chan struct{}) {
	rf.Lock()

	req := InstallSnapshotArgs{
		Term:              rf.term,
		LeaderId:          rf.me,
		LastIncludedIndex: rf.lastSnapshotIndex,
		LastIncludedTerm:  rf.lastSnapshotTerm,
		Data:              rf.persister.ReadSnapshot(),
	}
	reply := &InstallSnapshotReply{}

	rf.UnLock()

	// Send RPC
	request := func() bool {
		return rf.installSnapshot(peerIndex, req, reply)
	}

	ok := SendSnapshotRPCRequest(request)
	rf.Lock()
	defer rf.UnLock()
	if ok {

		if reply.Term > rf.term {
			rf.term = reply.Term
			rf.turnToFollow()
			return
		}

		if req.Term != rf.term {
			//log.Println("wrong term?? we skipped.")
			return
		}

		if rf.state != Leader {
			//log.Println("not leader?? we skipped.")
			return
		}

		rf.nextIndex[peerIndex] = req.LastIncludedIndex + 1
		//rf.matchIndex[peerIndex] = req.LastIncludedIndex

		sendAppendChan <- struct{}{} // Signal to leader-peer process that there may be appends to send
	}
}

// InstallSnapshot - RPC function
func (rf *Raft) InstallSnapshot(args *InstallSnapshotArgs, reply *InstallSnapshotReply) {
	rf.Lock()
	defer rf.UnLock()

	if rf.isDecommissioned {
		return
	}

	reply.Term = rf.term
	if args.Term < rf.term {
		return
	} else if args.Term >= rf.term {
		rf.term = args.Term
		rf.turnToFollow()
		rf.leaderID = args.LeaderId
		rf.votedFor = args.LeaderId
	}

	if rf.leaderID == args.LeaderId {
		rf.lastHeartBeat = time.Now()
	}
	rf.persist()
	DPrintf("%d receives installsnapshot, from leader %d, snapshot index %d, snapshot term %d, leader term %d, but my lastsnapshotindex %d, term %d",
		rf.me, args.LeaderId, args.LastIncludedIndex, args.LastIncludedTerm, args.Term, rf.lastSnapshotIndex, rf.term)

	if args.LastIncludedIndex > rf.lastSnapshotIndex {
		truncationStartIndex := rf.getOffsetIndex(args.LastIncludedIndex)
		rf.lastSnapshotIndex = args.LastIncludedIndex
		rf.lastSnapshotTerm = args.LastIncludedTerm // add snapshot term!
		oldCommitIndex := rf.commitIndex
		rf.commitIndex = Max(rf.commitIndex, rf.lastSnapshotIndex)

		if truncationStartIndex < len(rf.log) { // snapshot contain a prefix of its log
			rf.log = append(rf.log[truncationStartIndex:])
		} else { // snapshot contain new information not already in the follower's log
			rf.log = []Log{} // discards entire log
		}
		rf.persister.SaveStateAndSnapshot(rf.getPersistState(), args.Data)
		if rf.commitIndex > oldCommitIndex {
			rf.lastApplied = 0 // trigger loading snapshot
			rf.notifyApplyCh <- struct{}{}
		}
		rf.persist()
	}
}
