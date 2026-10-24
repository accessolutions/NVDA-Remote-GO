package server

import (
	"errors"
	"sort"
	"strconv"
	"sync"
	"time"
)

// A blockedClient is a machine the administrator refused from the dashboard.
//
// The block is keyed by IP address and channel together. Several machines often
// share one public address, each on its own channel, and the administrator must
// be able to refuse one of them without touching the others. The numeric client
// ID cannot serve as the key, since it changes at every connection; it is kept
// for the record, so the administrator can tell later which session led to the
// block.
type blockedClient struct {
	IP        string
	Channel   string
	ClientID  int
	BlockedAt time.Time
}

var blockList struct {
	sync.RWMutex
	entries map[string]blockedClient
}

var errClientNotFound = errors.New("no connected client has this identifier")

func blockKey(ip, channel string) string {
	return ip + "\x00" + channel
}

// isBlocked reports whether the given address is refused on the given channel.
func isBlocked(ip, channel string) bool {
	if ip == "" || channel == "" {
		return false
	}
	blockList.RLock()
	defer blockList.RUnlock()
	_, blocked := blockList.entries[blockKey(ip, channel)]
	return blocked
}

// blockedClients returns the current block list, most recent first.
func blockedClients() []blockedClient {
	blockList.RLock()
	list := make([]blockedClient, 0, len(blockList.entries))
	for _, entry := range blockList.entries {
		list = append(list, entry)
	}
	blockList.RUnlock()
	sort.Slice(list, func(i, j int) bool {
		if list[i].BlockedAt.Equal(list[j].BlockedAt) {
			if list[i].IP == list[j].IP {
				return list[i].Channel < list[j].Channel
			}
			return list[i].IP < list[j].IP
		}
		return list[i].BlockedAt.After(list[j].BlockedAt)
	})
	return list
}

// findClientByID returns the connected client carrying the given identifier.
func findClientByID(id int) *Client {
	list, _ := snapshotClients()
	for _, c := range list {
		if c.GetID() == id {
			return c
		}
	}
	return nil
}

// blockClientByID cuts the connection with the given identifier and refuses its
// address on its channel until unblock is called. Only that connection is cut:
// other sessions from the same address, on other channels, are left alone.
//
// A connection which has not joined a channel yet has nothing to be blocked on,
// so it is only cut. The returned flag tells whether a block was recorded.
func blockClientByID(id int) (blockedClient, bool, error) {
	target := findClientByID(id)
	if target == nil {
		return blockedClient{}, false, errClientNotFound
	}
	ip := target.GetIP()
	channel := ""
	if cc := target.GetChannel(); cc != nil {
		channel = cc.Name()
	}
	entry := blockedClient{
		IP:        ip,
		Channel:   channel,
		ClientID:  id,
		BlockedAt: time.Now(),
	}
	if channel == "" {
		Log(LOG_CONNECTION, "The administrator cut client "+strconv.Itoa(id)+" from "+ip+", which had not joined any channel; nothing was blocked.")
		target.Close()
		return entry, false, nil
	}

	blockList.Lock()
	if blockList.entries == nil {
		blockList.entries = make(map[string]blockedClient)
	}
	blockList.entries[blockKey(ip, channel)] = entry
	blockList.Unlock()

	storeBlockedClient(entry)
	Log(LOG_CONNECTION, "The administrator blocked client "+strconv.Itoa(id)+" from "+ip+" on channel "+channel+". This address can no longer join this channel.")
	target.Close()
	return entry, true, nil
}

// unblock lifts the block on an address for one channel. It reports whether
// anything changed.
func unblock(ip, channel string) bool {
	key := blockKey(ip, channel)
	blockList.Lock()
	_, existed := blockList.entries[key]
	if existed {
		delete(blockList.entries, key)
	}
	blockList.Unlock()
	if !existed {
		return false
	}
	deleteBlockedClient(ip, channel)
	Log(LOG_CONNECTION, "The administrator unblocked the address "+ip+" on channel "+channel+".")
	return true
}

// replaceBlockList swaps the whole list, used when the stored list is loaded at
// startup.
func replaceBlockList(entries []blockedClient) {
	fresh := make(map[string]blockedClient, len(entries))
	for _, entry := range entries {
		fresh[blockKey(entry.IP, entry.Channel)] = entry
	}
	blockList.Lock()
	blockList.entries = fresh
	blockList.Unlock()
}

// connectionRefusal tells whether a new NVDA Remote connection from the given
// address must be refused before anything is known about it, and why. An empty
// string means it may proceed.
//
// Until the administrator has defined the dashboard password, the server is not
// considered installed and accepts nobody, so that a freshly deployed relay
// cannot be used before anyone is able to supervise it. Blocks decided from the
// dashboard are checked later, by joinRefusal, once the channel is known.
func connectionRefusal(ip string) string {
	if AdminEnabled() && !adminPasswordDefined() {
		return "the administration password has not been defined yet"
	}
	return ""
}

// joinRefusal tells whether a client may join the given channel. An empty string
// means it may.
func joinRefusal(ip, channel string) string {
	if isBlocked(ip, channel) {
		return "the address " + ip + " is blocked on channel " + channel + " by the administrator"
	}
	return ""
}

// logRefusedConnection records a connection that was turned away.
func logRefusedConnection(ip, protocol, reason string) {
	Log(LOG_CONNECTION, "Refused a "+protocol+" connection from "+ip+": "+reason+".")
}
