package gossip

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"sync"
	"time"

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
