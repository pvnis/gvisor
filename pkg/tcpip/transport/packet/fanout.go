// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package packet

import (
	"hash/fnv"

	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// Fanout modes and flags from <linux/if_packet.h>.
const (
	fanoutHash     = 0
	fanoutLB       = 1
	fanoutCPU      = 2
	fanoutRollover = 3
	fanoutRND      = 4
	fanoutQM       = 5
	fanoutCBPF     = 6
	fanoutEBPF     = 7

	fanoutFlagRollover       = 0x1000
	fanoutFlagUniqueID       = 0x2000
	fanoutFlagIgnoreOutgoing = 0x4000
	fanoutFlagDefrag         = 0x8000
)

// fanoutGroup is a PACKET_FANOUT group: packet sockets of a stack that share
// a group ID, among which each frame is delivered to one member.
//
// Every mode spreads frames by flow hash (PACKET_FANOUT_HASH): each member
// independently computes the same choice for a frame, so that the stack's
// per-endpoint delivery delivers it once. That keeps a flow on one socket,
// which is what PACKET_FANOUT_HASH users such as DPDK's af_packet PMD rely
// on; the other modes balance differently on Linux but are only
// load-balancing policies. Rollover is not implemented.
type fanoutGroup struct {
	key  fanoutKey
	mode uint16

	mu sync.RWMutex
	// +checklocks:mu
	members []*endpoint
}

type fanoutKey struct {
	stack *stack.Stack
	id    uint16
}

var (
	fanoutGroupsMu sync.Mutex
	// +checklocks:fanoutGroupsMu
	fanoutGroups = map[fanoutKey]*fanoutGroup{}
)

// joinFanout adds ep to the fanout group described by arg, creating it if
// necessary, as net/packet/af_packet.c:fanout_add() does.
func joinFanout(ep *endpoint, arg uint32) (*fanoutGroup, tcpip.Error) {
	id := uint16(arg)
	mode := uint16(arg>>16) &^ (fanoutFlagRollover | fanoutFlagUniqueID | fanoutFlagIgnoreOutgoing | fanoutFlagDefrag)
	switch mode {
	case fanoutHash, fanoutLB, fanoutCPU, fanoutRollover, fanoutRND, fanoutQM:
	default:
		// BPF-steered fanout is not supported.
		return nil, &tcpip.ErrInvalidOptionValue{}
	}
	if uint16(arg>>16)&fanoutFlagUniqueID != 0 {
		return nil, &tcpip.ErrInvalidOptionValue{}
	}
	fanoutGroupsMu.Lock()
	defer fanoutGroupsMu.Unlock()
	key := fanoutKey{stack: ep.stack, id: id}
	g, ok := fanoutGroups[key]
	if !ok {
		g = &fanoutGroup{key: key, mode: mode}
		fanoutGroups[key] = g
	} else if g.mode != mode {
		return nil, &tcpip.ErrInvalidOptionValue{}
	}
	g.mu.Lock()
	g.members = append(g.members, ep)
	g.mu.Unlock()
	return g, nil
}

// leave removes ep from g, deleting g when it becomes empty.
func (g *fanoutGroup) leave(ep *endpoint) {
	fanoutGroupsMu.Lock()
	defer fanoutGroupsMu.Unlock()
	g.mu.Lock()
	for i, m := range g.members {
		if m == ep {
			g.members = append(g.members[:i], g.members[i+1:]...)
			break
		}
	}
	empty := len(g.members) == 0
	g.mu.Unlock()
	if empty && fanoutGroups[g.key] == g {
		delete(fanoutGroups, g.key)
	}
}

// accepts returns whether ep, a member of g, is the member that receives
// pkt.
func (g *fanoutGroup) accepts(ep *endpoint, pkt *stack.PacketBuffer) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	n := len(g.members)
	if n <= 1 {
		return true
	}
	return g.members[flowHash(pkt)%uint32(n)] == ep
}

// flowHash hashes the flow a frame belongs to: its IP addresses and, for TCP
// and UDP, ports; or, for other protocols (e.g. eCPRI), its Ethernet
// addresses and type.
func flowHash(pkt *stack.PacketBuffer) uint32 {
	v := stack.PayloadSince(pkt.LinkHeader())
	defer v.Release()
	b := v.AsSlice()
	h := fnv.New32a()
	if len(b) < header.EthernetMinimumSize {
		h.Write(b)
		return h.Sum32()
	}
	eth := header.Ethernet(b)
	l3 := b[header.EthernetMinimumSize:]
	switch eth.Type() {
	case header.IPv4ProtocolNumber:
		if ip := header.IPv4(l3); ip.IsValid(len(l3)) {
			h.Write(ip[12:20])
			if hl := int(ip.HeaderLength()); (ip.Protocol() == uint8(header.TCPProtocolNumber) || ip.Protocol() == uint8(header.UDPProtocolNumber)) && len(l3) >= hl+4 && ip.FragmentOffset() == 0 {
				h.Write(l3[hl : hl+4])
			}
			return h.Sum32()
		}
	case header.IPv6ProtocolNumber:
		if len(l3) >= header.IPv6MinimumSize {
			ip := header.IPv6(l3)
			h.Write(l3[8:40])
			if nh := ip.NextHeader(); (nh == uint8(header.TCPProtocolNumber) || nh == uint8(header.UDPProtocolNumber)) && len(l3) >= header.IPv6MinimumSize+4 {
				h.Write(l3[header.IPv6MinimumSize : header.IPv6MinimumSize+4])
			}
			return h.Sum32()
		}
	}
	h.Write(b[:header.EthernetMinimumSize])
	return h.Sum32()
}
