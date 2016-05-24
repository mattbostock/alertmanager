package gossip

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/prometheus/alertmanager/provider"
	"github.com/prometheus/alertmanager/types"
	"github.com/prometheus/common/log"
	"github.com/prometheus/common/model"
	"github.com/weaveworks/mesh"
)

func NewNotificationInfo(name mesh.PeerName, logger log.Logger) *NotificationInfo {
	ni := &NotificationInfo{
		st: &notificationState{
			set: map[string]notificationEntry{},
		},
		logger: logger,
	}
	go func() {
		for range time.Tick(10 * time.Second) {
			logger.Infoln("status", ni.st.set)
		}
	}()
	return ni
}

type NotificationInfo struct {
	mtx    sync.RWMutex
	st     *notificationState
	send   mesh.Gossip
	logger log.Logger
}

func (ni *NotificationInfo) Register(g mesh.Gossip) {
	ni.mtx.Lock()
	defer ni.mtx.Unlock()
	ni.send = g
}

func (ni *NotificationInfo) Gossip() mesh.GossipData {
	ni.mtx.RLock()
	defer ni.mtx.RUnlock()
	complete := ni.st.copy()

	ni.logger.Infoln("Gossip => complete %v", complete)
	return complete
}

func (ni *NotificationInfo) OnGossip(buf []byte) (delta mesh.GossipData, err error) {
	var set map[string]notificationEntry
	if err := gob.NewDecoder(bytes.NewReader(buf)).Decode(&set); err != nil {
		return nil, err
	}

	ni.mtx.RLock()
	ni.st.mtx.Lock()

	for k, v := range set {
		if prev, ok := ni.st.set[k]; ok {
			if v.Timestamp.Before(prev.Timestamp) {
				delete(set, k)
				continue
			}
		}
		ni.st.set[k] = v
	}

	ni.st.mtx.Unlock()
	ni.mtx.RUnlock()

	ni.logger.Infoln("OnGossip %v => delta %v", set, set)

	if len(set) == 0 {
		return nil, nil
	}
	return &notificationState{set: set}, nil
}

func (ni *NotificationInfo) OnGossipBroadcast(src mesh.PeerName, buf []byte) (received mesh.GossipData, err error) {
	var set map[string]notificationEntry
	if err := gob.NewDecoder(bytes.NewReader(buf)).Decode(&set); err != nil {
		return nil, err
	}

	ni.mtx.RLock()
	ni.st.mtx.Lock()

	for k, v := range set {
		if prev, ok := ni.st.set[k]; ok {
			if v.Timestamp.Before(prev.Timestamp) {
				delete(set, k)
				continue
			}
		}
		ni.st.set[k] = v
	}

	ni.st.mtx.Unlock()
	ni.mtx.RUnlock()

	return &notificationState{set: set}, nil
}

func (ni *NotificationInfo) OnGossipUnicast(src mesh.PeerName, buf []byte) error {
	var set map[string]notificationEntry
	if err := gob.NewDecoder(bytes.NewReader(buf)).Decode(&set); err != nil {
		return err
	}

	ni.mtx.RLock()
	ni.st.mtx.Lock()

	for k, v := range set {
		if prev, ok := ni.st.set[k]; ok {
			if v.Timestamp.Before(prev.Timestamp) {
				continue
			}
		}
		ni.st.set[k] = v
	}

	ni.st.mtx.Unlock()
	ni.mtx.RUnlock()
	return nil
}

func (ni *NotificationInfo) Get(dest string, fps ...model.Fingerprint) ([]*types.NotifyInfo, error) {
	ni.st.mtx.RLock()
	defer ni.st.mtx.RUnlock()

	res := make([]*types.NotifyInfo, 0, len(fps))

	for _, fp := range fps {
		k := fmt.Sprintf("%x:%s", fp, dest)
		if e, ok := ni.st.set[k]; ok {
			res = append(res, &types.NotifyInfo{
				Alert:     fp,
				Receiver:  dest,
				Resolved:  e.Resolved,
				Timestamp: e.Timestamp,
			})
		} else {
			res = append(res, nil)
		}
	}
	return res, nil
}

func (ni *NotificationInfo) Set(ns ...*types.NotifyInfo) error {
	update := &notificationState{
		set: map[string]notificationEntry{},
	}

	for _, n := range ns {
		k := fmt.Sprintf("%x:%s", n.Alert, n.Receiver)
		update.set[k] = notificationEntry{
			Resolved:  n.Resolved,
			Timestamp: n.Timestamp,
		}
	}

	ni.mtx.Lock()
	ni.st = ni.st.Merge(update).(*notificationState)
	ni.mtx.Unlock()

	ni.send.GossipBroadcast(update)
	return nil
}

type notificationEntry struct {
	Resolved  bool
	Timestamp time.Time
}

type notificationState struct {
	mtx sync.RWMutex
	set map[string]notificationEntry
}

func (s *notificationState) copy() *notificationState {
	s.mtx.RLock()
	defer s.mtx.RUnlock()

	res := &notificationState{
		set: make(map[string]notificationEntry, len(s.set)),
	}
	for k, v := range s.set {
		res.set[k] = v
	}
	return res
}

func (st *notificationState) Encode() [][]byte {
	st.mtx.RLock()
	defer st.mtx.RUnlock()

	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(&st.set); err != nil {
		panic(err)
	}
	return [][]byte{buf.Bytes()}
}

func (st *notificationState) Merge(other mesh.GossipData) mesh.GossipData {
	res := st.copy()

	for k, v := range other.(*notificationState).set {
		prev, ok := res.set[k]
		if !ok {
			res.set[k] = v
			continue
		}
		if prev.Timestamp.Before(v.Timestamp) {
			res.set[k] = v
		}
	}
	return res
}

type Silences struct {
	mtx    sync.RWMutex
	logger log.Logger
	st     *silenceState
	send   mesh.Gossip
	mk     types.Marker
}

func NewSilences(_ mesh.PeerName, mk types.Marker, logger log.Logger) *Silences {
	return &Silences{
		mk:     mk,
		logger: logger,
		st: &silenceState{
			set: map[uint64]*model.Silence{},
		},
	}
}

func (s *Silences) Register(g mesh.Gossip) {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	s.send = g
}

func (s *Silences) Mutes(lset model.LabelSet) bool {
	sils, err := s.All()
	if err != nil {
		log.Errorf("retrieving silences failed: %s", err)
		// In doubt, do not silence anything.
		return false
	}

	for _, sil := range sils {
		s.logger.Infoln("check", sil)
		if sil.Mutes(lset) {
			s.mk.SetSilenced(lset.Fingerprint(), sil.ID)
			return true
		}
	}

	s.mk.SetSilenced(lset.Fingerprint())
	return false
}

func (s *Silences) All() ([]*types.Silence, error) {
	s.mtx.RLock()
	defer s.mtx.RUnlock()

	res := make([]*types.Silence, 0, len(s.st.set))

	s.st.mtx.RLock()
	defer s.st.mtx.RUnlock()

	s.logger.Infoln("silences", s.st.set)
	for _, sil := range s.st.set {
		res = append(res, types.NewSilence(sil))
	}

	return res, nil
}

func (s *Silences) Set(sil *types.Silence) (uint64, error) {
	// TODO(fabxc): replace with UUIDs!!!
	if sil.ID == 0 {
		sil.ID = uint64(rand.Int63())
	}

	s.mtx.RLock()

	update := &silenceState{
		set: map[uint64]*model.Silence{
			sil.ID: &sil.Silence,
		},
	}

	// TODO(fabxc): add an updatedAt timestamp.
	s.st = s.st.Merge(update).(*silenceState)

	s.mtx.RUnlock()

	s.send.GossipBroadcast(update)

	return sil.ID, nil
}

func (s *Silences) Del(id uint64) error {
	// XXX(fabxc): we likely want to switch to an approach in which deleting
	// means to set the end timestamp to now.
	panic("not implemented")
}

func (s *Silences) Get(id uint64) (*types.Silence, error) {
	s.mtx.RLock()
	s.st.mtx.RLock()

	defer func() {
		s.st.mtx.RUnlock()
		s.mtx.RUnlock()
	}()

	sil, ok := s.st.set[id]
	if !ok {
		return nil, provider.ErrNotFound
	}
	return types.NewSilence(sil), nil
}

func (s *Silences) Gossip() mesh.GossipData {
	s.mtx.RLock()
	defer s.mtx.RUnlock()
	return s.st.copy()
}

func (s *Silences) OnGossip(update []byte) (mesh.GossipData, error) {
	var set map[uint64]*model.Silence
	if err := gob.NewDecoder(bytes.NewReader(update)).Decode(&set); err != nil {
		return nil, err
	}

	s.mtx.RLock()
	s.st.mtx.Lock()

	for k, v := range set {
		if prev, ok := s.st.set[k]; ok {
			if v.CreatedAt.Before(prev.CreatedAt) {
				delete(set, k)
				continue
			}
		}
		s.st.set[k] = v
	}

	s.st.mtx.Unlock()
	s.mtx.RUnlock()

	if len(set) == 0 {
		return nil, nil
	}
	return &silenceState{set: set}, nil
}

func (s *Silences) OnGossipBroadcast(p mesh.PeerName, update []byte) (mesh.GossipData, error) {
	var set map[uint64]*model.Silence
	if err := gob.NewDecoder(bytes.NewReader(update)).Decode(&set); err != nil {
		return nil, err
	}

	s.mtx.RLock()
	s.st.mtx.Lock()

	for k, v := range set {
		if prev, ok := s.st.set[k]; ok {
			if v.CreatedAt.Before(prev.CreatedAt) {
				delete(set, k)
				continue
			}
		}
		s.st.set[k] = v
	}

	s.st.mtx.Unlock()
	s.mtx.RUnlock()
	return &silenceState{set: set}, nil
}

func (s *Silences) OnGossipUnicast(sender mesh.PeerName, update []byte) error {
	var set map[uint64]*model.Silence
	if err := gob.NewDecoder(bytes.NewReader(update)).Decode(&set); err != nil {
		return err
	}

	s.mtx.RLock()
	s.st.mtx.Lock()

	for k, v := range set {
		if prev, ok := s.st.set[k]; ok {
			if v.CreatedAt.Before(prev.CreatedAt) {
				continue
			}
		}
		s.st.set[k] = v
	}

	s.st.mtx.Unlock()
	s.mtx.RUnlock()
	return nil
}

type silenceState struct {
	mtx sync.RWMutex
	set map[uint64]*model.Silence
}

func (st *silenceState) Encode() [][]byte {
	st.mtx.RLock()
	defer st.mtx.RUnlock()

	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(&st.set); err != nil {
		panic(err)
	}
	return [][]byte{buf.Bytes()}

}

func (st *silenceState) Merge(other mesh.GossipData) mesh.GossipData {
	res := st.copy()

	for k, v := range other.(*silenceState).set {
		prev, ok := res.set[k]
		if !ok {
			res.set[k] = v
			continue
		}
		// This theoretically allows modifying an existing silence.
		// In practice we currently keep silences immutable and delete/create
		// instead of modifying.
		// It should probably have an UpdatedAt field here instead.
		if prev.CreatedAt.Before(v.CreatedAt) {
			res.set[k] = v
		}
	}
	return res
}

func (s *silenceState) copy() *silenceState {
	s.mtx.RLock()
	defer s.mtx.RUnlock()

	res := &silenceState{
		set: make(map[uint64]*model.Silence, len(s.set)),
	}
	for k, v := range s.set {
		res.set[k] = v
	}
	return res
}
