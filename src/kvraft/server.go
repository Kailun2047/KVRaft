package raftkv

import (
	"bytes"
	"labgob"
	"labrpc"
	"log"
	"raft"
	"sync"
	"time"
)

const (
	GetType    = "Get"
	PutType    = "Put"
	AppendType = "Append"
)

type Op struct {
	// Your definitions here.
	// Field names must start with capital letters,
	// otherwise RPC will break.
	Type      string
	Key       string
	Value     string
	CommandID string
}

type KVServer struct {
	mu      sync.Mutex
	me      int
	rf      *raft.Raft
	applyCh chan raft.ApplyMsg

	maxraftstate int // snapshot if log grows this big

	// Your definitions here.
	executed         map[string]struct{} // Record executed command numbers.
	pairs            map[string]string
	cond             *sync.Cond
	applied          map[int]chan string
	enableDebug      bool
	lastCommandIndex int // Used for snapshotting.
	snapshottedIndex int // Used for snapshotting.
}

func (kv *KVServer) Get(args *GetArgs, reply *GetReply) {
	// Your code here.
	kv.mu.Lock()
	index, _, isLeader := kv.rf.Start(Op{
		Type:      GetType,
		Key:       args.Key,
		Value:     "",
		CommandID: args.CommandID,
	})
	if !isLeader {
		reply.WrongLeader = true
		kv.mu.Unlock()
		return
	}
	kv.debug("Leader %d receives Get request %v\n", kv.me, *args)
	if _, ok := kv.executed[args.CommandID]; ok {
		reply.Err = ""
		reply.WrongLeader = false
		reply.Value = kv.pairs[args.Key]
		kv.mu.Unlock()
		return
	}
	if _, ok := kv.applied[index]; !ok {
		kv.applied[index] = make(chan string)
	}
	kv.mu.Unlock()
	commandID := <-kv.applied[index]
	if commandID != args.CommandID {
		reply.Err = "Leadership changed before commit (new leader applied new command)"
		reply.WrongLeader = true
		return
	}
	reply.WrongLeader = false
	kv.mu.Lock()
	defer kv.mu.Unlock()
	reply.Value = kv.pairs[args.Key]
	delete(kv.applied, index)
}

func (kv *KVServer) PutAppend(args *PutAppendArgs, reply *PutAppendReply) {
	// Your code here.
	kv.mu.Lock()
	index, _, isLeader := kv.rf.Start(Op{
		Type:      args.Op,
		Key:       args.Key,
		Value:     args.Value,
		CommandID: args.CommandID,
	})
	if !isLeader {
		reply.WrongLeader = true
		kv.mu.Unlock()
		return
	}
	kv.debug("Leader %d receives PutAppend request %v\n", kv.me, *args)
	if _, ok := kv.executed[args.CommandID]; ok {
		reply.Err = ""
		reply.WrongLeader = false
		kv.mu.Unlock()
		return
	}
	if _, ok := kv.applied[index]; !ok {
		kv.applied[index] = make(chan string)
	}
	kv.mu.Unlock()
	commandID := <-kv.applied[index]
	kv.mu.Lock()
	defer kv.mu.Unlock()
	delete(kv.applied, index)
	if commandID != args.CommandID {
		reply.Err = "Leadership changed before commit (new leader applied new command)"
		reply.WrongLeader = true
		return
	}
	reply.WrongLeader = false
}

//
// the tester calls Kill() when a KVServer instance won't
// be needed again. you are not required to do anything
// in Kill(), but it might be convenient to (for example)
// turn off debug output from this instance.
//
func (kv *KVServer) Kill() {
	kv.rf.Kill()
	// Your code here, if desired.
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
	// call labgob.Register on structures you want
	// Go's RPC library to marshall/unmarshall.
	labgob.Register(Op{})

	kv := new(KVServer)
	kv.me = me
	kv.maxraftstate = maxraftstate

	// You may need initialization code here.
	kv.executed = make(map[string]struct{})
	kv.pairs = make(map[string]string)

	kv.applyCh = make(chan raft.ApplyMsg)
	kv.rf = raft.Make(servers, me, persister, kv.applyCh)

	// You may need initialization code here.
	kv.applied = make(map[int]chan string)
	kv.enableDebug = false
	snapshotBytes := kv.rf.GetSnapshot()

	if len(snapshotBytes) > 0 {
		buf := bytes.NewBuffer(snapshotBytes)
		decoder := labgob.NewDecoder(buf)
		err := decoder.Decode(&kv.lastCommandIndex)
		kv.snapshottedIndex = kv.lastCommandIndex
		if err != nil {
			log.Printf("Error decoding last included index: [%v]\n", err)
		}
		var lastIncludedTerm int
		err = decoder.Decode(&lastIncludedTerm)
		if err != nil {
			log.Printf("Error decoding last included term: [%v]\n", err)
		}
		err = decoder.Decode(&kv.pairs)
		if err != nil {
			log.Printf("Error decoding k-v pairs: [%v]", err)
		}
	}

	go readAppliedCommand(kv)
	if kv.maxraftstate >= 0 {
		go checkRaftStateSize(kv)
	}

	return kv
}

func readAppliedCommand(kv *KVServer) {
	for {
		time.Sleep(10 * time.Millisecond)
		applyMsg := <-kv.applyCh
		if applyMsg.CommandValid {
			op := applyMsg.Command.(Op)
			kv.mu.Lock()
			if _, ok := kv.executed[op.CommandID]; !ok {
				switch op.Type {
				case PutType:
					kv.pairs[op.Key] = op.Value
				case AppendType:
					kv.pairs[op.Key] = kv.pairs[op.Key] + op.Value
				}
				kv.executed[op.CommandID] = struct{}{}
			}
			kv.lastCommandIndex = applyMsg.CommandIndex
			kv.mu.Unlock()
			if ch, ok := kv.applied[applyMsg.CommandIndex]; ok {
				ch <- op.CommandID
			}
		}
	}
}

func checkRaftStateSize(kv *KVServer) {
	for {
		time.Sleep(10 * time.Millisecond)
		kv.mu.Lock()
		if kv.rf.GetStateSize() > kv.maxraftstate && kv.lastCommandIndex > kv.snapshottedIndex {
			kv.rf.CreateSnapshot(kv.pairs, kv.lastCommandIndex)
			kv.snapshottedIndex = kv.lastCommandIndex
		}
		kv.mu.Unlock()
	}
}

func (kv *KVServer) debug(s string, a ...interface{}) {
	if kv.enableDebug {
		log.Printf(s, a...)
	}
}
