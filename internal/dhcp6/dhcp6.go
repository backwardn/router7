// Copyright 2018 Google Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package dhcp6 implements a DHCPv6 client.
package dhcp6

import (
	"fmt"
	"log"
	"net"
	"strconv"
	"time"

	"github.com/insomniacslk/dhcp/dhcpv6"
	"github.com/insomniacslk/dhcp/dhcpv6/client6"
	"github.com/insomniacslk/dhcp/iana"
)

type ClientConfig struct {
	InterfaceName string // e.g. eth0

	// LocalAddr allows overwriting the source address used for sending DHCPv6
	// packets. It defaults to the first link-local address of InterfaceName.
	LocalAddr *net.UDPAddr

	// RemoteAddr allows addressing a specific DHCPv6 server. It defaults to
	// the dhcpv6.AllDHCPRelayAgentsAndServers multicast address.
	RemoteAddr *net.UDPAddr

	// DUID contains all bytes (including the prefixing uint16 type field) for a
	// DHCP Unique Identifier (e.g. []byte{0x00, 0x0a, 0x00, 0x03, 0x00, 0x01,
	// 0x4c, 0x5e, 0xc, 0x41, 0xbf, 0x39}).
	//
	// Fiber7 assigns static IPv6 /48 networks to DUIDs, so it is important to
	// be able to carry it around between devices.
	DUID []byte

	Conn           net.PacketConn         // for testing
	TransactionIDs []dhcpv6.TransactionID // for testing

	// HardwareAddr allows overriding the hardware address in tests. If nil,
	// defaults to the hardware address of the interface identified by
	// InterfaceName.
	HardwareAddr net.HardwareAddr
}

// Config contains the obtained network configuration.
type Config struct {
	RenewAfter time.Time   `json:"valid_until"`
	Prefixes   []net.IPNet `json:"prefixes"` // e.g. 2a02:168:4a00::/48
	DNS        []string    `json:"dns"`      // e.g. 2001:1620:2777:1::10, 2001:1620:2777:2::20
}

type Client struct {
	interfaceName string
	hardwareAddr  net.HardwareAddr
	raddr         *net.UDPAddr
	timeNow       func() time.Time
	duid          *dhcpv6.Duid
	advertise     *dhcpv6.Message

	cfg Config
	err error

	Conn           net.PacketConn // TODO: unexport
	transactionIDs []dhcpv6.TransactionID

	ReadTimeout  time.Duration
	WriteTimeout time.Duration

	RemoteAddr net.Addr
}

func NewClient(cfg ClientConfig) (*Client, error) {
	iface, err := net.InterfaceByName(cfg.InterfaceName)
	if err != nil {
		return nil, err
	}

	// if no LocalAddr is specified, get the interface's link-local address
	laddr := cfg.LocalAddr
	if laddr == nil {
		llAddr, err := dhcpv6.GetLinkLocalAddr(cfg.InterfaceName)
		if err != nil {
			return nil, err
		}
		laddr = &net.UDPAddr{
			IP:   llAddr,
			Port: dhcpv6.DefaultClientPort,
			// HACK: Zone should ideally be cfg.InterfaceName, but Go’s
			// ipv6ZoneCache is only updated every 60s, so the addition of the
			// veth interface will not be picked up for all tests after the
			// first test.
			Zone: strconv.Itoa(iface.Index),
		}
	}

	// if no RemoteAddr is specified, use AllDHCPRelayAgentsAndServers
	raddr := cfg.RemoteAddr
	if raddr == nil {
		raddr = &net.UDPAddr{
			IP:   dhcpv6.AllDHCPRelayAgentsAndServers,
			Port: dhcpv6.DefaultServerPort,
		}
	}

	hardwareAddr := iface.HardwareAddr
	if cfg.HardwareAddr != nil {
		hardwareAddr = cfg.HardwareAddr
	}

	var duid *dhcpv6.Duid
	if cfg.DUID != nil {
		var err error
		duid, err = dhcpv6.DuidFromBytes(cfg.DUID)
		if err != nil {
			return nil, err
		}
		fmt.Printf("duid: %T, %v, %#v", duid, duid, duid)
	} else {
		duid = &dhcpv6.Duid{
			Type:          dhcpv6.DUID_LLT,
			HwType:        iana.HWTypeEthernet,
			Time:          dhcpv6.GetTime(),
			LinkLayerAddr: hardwareAddr,
		}
	}

	// prepare the socket to listen on for replies
	conn := cfg.Conn
	if conn == nil {
		udpConn, err := net.ListenUDP("udp6", laddr)
		if err != nil {
			return nil, err
		}
		conn = udpConn
	}

	return &Client{
		interfaceName:  cfg.InterfaceName,
		hardwareAddr:   hardwareAddr,
		timeNow:        time.Now,
		raddr:          raddr,
		Conn:           conn,
		duid:           duid,
		transactionIDs: cfg.TransactionIDs,
		ReadTimeout:    client6.DefaultReadTimeout,
		WriteTimeout:   client6.DefaultWriteTimeout,
	}, nil
}

func (c *Client) Close() error {
	return c.Conn.Close()
}

const maxUDPReceivedPacketSize = 8192 // arbitrary size. Theoretically could be up to 65kb

func (c *Client) sendReceive(packet *dhcpv6.Message, expectedType dhcpv6.MessageType) (*dhcpv6.Message, error) {
	if packet == nil {
		return nil, fmt.Errorf("packet to send cannot be nil")
	}
	if expectedType == dhcpv6.MessageTypeNone {
		// infer the expected type from the packet being sent
		if packet.Type() == dhcpv6.MessageTypeSolicit {
			expectedType = dhcpv6.MessageTypeAdvertise
		} else if packet.Type() == dhcpv6.MessageTypeRequest {
			expectedType = dhcpv6.MessageTypeReply
		} else if packet.Type() == dhcpv6.MessageTypeRelayForward {
			expectedType = dhcpv6.MessageTypeRelayReply
		} else if packet.Type() == dhcpv6.MessageTypeLeaseQuery {
			expectedType = dhcpv6.MessageTypeLeaseQueryReply
		} // and probably more
	}

	// send the packet out
	c.Conn.SetWriteDeadline(time.Now().Add(c.WriteTimeout))
	if _, err := c.Conn.WriteTo(packet.ToBytes(), c.raddr); err != nil {
		return nil, err
	}

	// wait for a reply
	c.Conn.SetReadDeadline(time.Now().Add(c.ReadTimeout))
	var (
		adv *dhcpv6.Message
	)
	for {
		buf := make([]byte, maxUDPReceivedPacketSize)
		n, _, err := c.Conn.ReadFrom(buf)
		if err != nil {
			return nil, err
		}
		adv, err = dhcpv6.MessageFromBytes(buf[:n])
		if err != nil {
			log.Printf("non-DHCP: %v", err)
			// skip non-DHCP packets
			continue
		}
		if packet.TransactionID != adv.TransactionID {
			log.Printf("different XID: got %v, want %v", adv.TransactionID, packet.TransactionID)
			// different XID, we don't want this packet for sure
			continue
		}
		if expectedType == dhcpv6.MessageTypeNone {
			// just take whatever arrived
			break
		} else if adv.MessageType == expectedType {
			break
		}
	}
	return adv, nil
}

func (c *Client) solicit(solicit *dhcpv6.Message) (*dhcpv6.Message, *dhcpv6.Message, error) {
	var err error
	if solicit == nil {
		solicit, err = dhcpv6.NewSolicit(c.hardwareAddr, dhcpv6.WithClientID(*c.duid))
		if err != nil {
			return nil, nil, err
		}
	}
	if len(c.transactionIDs) > 0 {
		id := c.transactionIDs[0]
		c.transactionIDs = c.transactionIDs[1:]
		solicit.TransactionID = id
	}
	solicit.AddOption(&dhcpv6.OptIAPD{IaId: [4]byte{0, 0, 0, 1}})
	advertise, err := c.sendReceive(solicit, dhcpv6.MessageTypeNone)
	return solicit, advertise, err
}

func (c *Client) request(advertise *dhcpv6.Message) (*dhcpv6.Message, *dhcpv6.Message, error) {
	request, err := dhcpv6.NewRequestFromAdvertise(advertise, dhcpv6.WithClientID(*c.duid))
	if err != nil {
		return nil, nil, err
	}
	if iapd := advertise.Options.OneIAPD(); iapd != nil {
		request.AddOption(iapd)
	}

	if len(c.transactionIDs) > 0 {
		id := c.transactionIDs[0]
		c.transactionIDs = c.transactionIDs[1:]
		request.TransactionID = id
	}
	reply, err := c.sendReceive(request, dhcpv6.MessageTypeNone)
	return request, reply, err
}

func (c *Client) ObtainOrRenew() bool {
	c.err = nil // clear previous error
	_, advertise, err := c.solicit(nil)
	if err != nil {
		c.err = err
		return true
	}

	c.advertise = advertise
	_, reply, err := c.request(advertise)
	if err != nil {
		c.err = err
		return true
	}
	var newCfg Config
	for _, iapd := range reply.Options.IAPD() {
		t1 := c.timeNow().Add(iapd.T1)
		if t1.Before(newCfg.RenewAfter) || newCfg.RenewAfter.IsZero() {
			newCfg.RenewAfter = t1
		}
		for _, prefix := range iapd.Options.Prefixes() {
			newCfg.Prefixes = append(newCfg.Prefixes, *prefix.Prefix)
		}
	}
	for _, dns := range reply.Options.DNS() {
		newCfg.DNS = append(newCfg.DNS, dns.String())
	}
	c.cfg = newCfg
	return true
}

func (c *Client) Release() (release *dhcpv6.Message, reply *dhcpv6.Message, err error) {
	release, err = dhcpv6.NewRequestFromAdvertise(c.advertise, dhcpv6.WithClientID(*c.duid))
	if err != nil {
		return nil, nil, err
	}
	release.MessageType = dhcpv6.MessageTypeRelease

	if len(c.transactionIDs) > 0 {
		id := c.transactionIDs[0]
		c.transactionIDs = c.transactionIDs[1:]
		release.TransactionID = id
	}
	reply, err = c.sendReceive(release, dhcpv6.MessageTypeNone)
	return release, reply, err
}

func (c *Client) Err() error {
	return c.err
}

func (c *Client) Config() Config {
	return c.cfg
}
