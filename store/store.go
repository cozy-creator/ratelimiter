package store

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/hashicorp/memberlist"
)

// Entry represents a single key-value pair with version info
type Entry struct {
	Value     []byte    `json:"value"`
	Version   int64     `json:"version"`
	LastWrite time.Time `json:"last_write"`
}

// Message represents a gossip message for state synchronization
type Message struct {
	Key     string `json:"key"`
	Entry   Entry  `json:"entry"`
	NodeID  string `json:"node_id"`
	IsDelete bool  `json:"is_delete"`
}

// Store implements a distributed key-value store with eventual consistency
type Store struct {
	mu       sync.RWMutex
	data     map[string]Entry
	nodeID   string
	members  *memberlist.Memberlist
	updates  chan Message
	onChange func(key string, value []byte)
	delegate *delegate
}

// NewStore creates a new distributed store instance
func NewStore(bindAddr string, knownNodes []string, onChange func(key string, value []byte)) (*Store, error) {
	d := &delegate{
		broadcasts: make([][]byte, 0),
	}

	s := &Store{
		data:     make(map[string]Entry),
		nodeID:   bindAddr,
		updates:  make(chan Message, 1000),
		onChange: onChange,
		delegate: d,
	}
	d.store = s

	config := memberlist.DefaultLocalConfig()
	config.BindAddr = bindAddr
	config.Name = bindAddr
	config.Delegate = d

	list, err := memberlist.Create(config)
	if err != nil {
		return nil, fmt.Errorf("failed to create memberlist: %v", err)
	}

	if len(knownNodes) > 0 {
		_, err = list.Join(knownNodes)
		if err != nil {
			log.Printf("failed to join cluster: %v", err)
		}
	}

	s.members = list

	// Start gossip handler
	go s.handleUpdates()

	return s, nil
}

// Get retrieves a value from the store
func (s *Store) Get(key string) ([]byte, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if entry, ok := s.data[key]; ok {
		return entry.Value, true
	}
	return nil, false
}

// Set stores a value with versioning
func (s *Store) Set(key string, value []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	entry := Entry{
		Value:     value,
		Version:   time.Now().UnixNano(),
		LastWrite: time.Now(),
	}

	s.data[key] = entry

	// Broadcast update
	msg := Message{
		Key:    key,
		Entry:  entry,
		NodeID: s.nodeID,
	}

	return s.broadcast(msg)
}

// Delete removes a key from the store
func (s *Store) Delete(key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.data, key)

	msg := Message{
		Key:      key,
		NodeID:   s.nodeID,
		IsDelete: true,
	}

	return s.broadcast(msg)
}

func (s *Store) broadcast(msg Message) error {
	msgBytes, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal message: %v", err)
	}

	s.delegate.mu.Lock()
	s.delegate.broadcasts = append(s.delegate.broadcasts, msgBytes)
	s.delegate.mu.Unlock()
	return nil
}

func (s *Store) handleUpdates() {
	for msg := range s.updates {
		s.mu.Lock()
		existing, exists := s.data[msg.Key]
		
		if msg.IsDelete {
			if !exists || existing.Version < msg.Entry.Version {
				delete(s.data, msg.Key)
			}
		} else {
			if !exists || existing.Version < msg.Entry.Version {
				s.data[msg.Key] = msg.Entry
				if s.onChange != nil {
					s.onChange(msg.Key, msg.Entry.Value)
				}
			}
		}
		s.mu.Unlock()
	}
}

// delegate implements memberlist.Delegate interface
type delegate struct {
	store *Store
	broadcasts [][]byte
	mu    sync.RWMutex
}

func (d *delegate) NodeMeta(limit int) []byte {
	meta := map[string]string{
		"id": d.store.nodeID,
	}
	bytes, _ := json.Marshal(meta)
	return bytes
}

func (d *delegate) NotifyMsg(msg []byte) {
	var message Message
	if err := json.Unmarshal(msg, &message); err != nil {
		log.Printf("failed to unmarshal message: %v", err)
		return
	}
	d.store.updates <- message
}

func (d *delegate) GetBroadcasts(overhead, limit int) [][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()

	if len(d.broadcasts) == 0 {
		return nil
	}

	count := min64(int64(len(d.broadcasts)), int64(limit))
	msgs := make([][]byte, 0, count)
	msgs = append(msgs, d.broadcasts[:count]...)
	d.broadcasts = d.broadcasts[count:]
	return msgs
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func (d *delegate) LocalState(join bool) []byte {
	d.store.mu.RLock()
	defer d.store.mu.RUnlock()

	state := make(map[string]Entry)
	for k, v := range d.store.data {
		state[k] = v
	}

	bytes, err := json.Marshal(state)
	if err != nil {
		log.Printf("failed to marshal local state: %v", err)
		return nil
	}
	return bytes
}

func (d *delegate) MergeRemoteState(buf []byte, join bool) {
	if len(buf) == 0 {
		return
	}

	var remoteState map[string]Entry
	if err := json.Unmarshal(buf, &remoteState); err != nil {
		log.Printf("failed to unmarshal remote state: %v", err)
		return
	}

	d.store.mu.Lock()
	defer d.store.mu.Unlock()

	for k, v := range remoteState {
		existing, exists := d.store.data[k]
		if !exists || existing.Version < v.Version {
			d.store.data[k] = v
			if d.store.onChange != nil {
				d.store.onChange(k, v.Value)
			}
		}
	}
}

