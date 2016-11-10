// Copyright © 2016 The Things Network
// Use of this source code is governed by the MIT license that can be found in the LICENSE file.

package exchange

import (
	"fmt"
	"sync"

	"github.com/TheThingsNetwork/gateway-connector-bridge/backend"
	"github.com/TheThingsNetwork/gateway-connector-bridge/types"
	"github.com/apex/log"
)

// Exchange routes messages between northbound backends (servers that are up the chain)
// and southbound backends gateways or servers that are down the chain.
//
// When a connection message is received on the southbound backend:
// - Uplink messages are routed from the southbound backends to the northbound backends
// - Downlink messages are routed from the northbound backends to the southbound backends
// - Status messages are routed from the southbound backends to the northbound backends
// until a disconnection message is received on the southbound backend
type Exchange struct {
	ctx  log.Interface
	mu   sync.Mutex
	done chan struct{}

	northboundBackends []backend.Northbound
	southboundBackends []backend.Southbound

	northboundDone map[string][]chan struct{}
	southboundDone map[string][]chan struct{}
	doneLock       sync.Mutex

	connect    chan *types.ConnectMessage
	disconnect chan *types.DisconnectMessage
	uplink     chan *types.UplinkMessage
	status     chan *types.StatusMessage
	downlink   chan *types.DownlinkMessage
}

// New initializes a new Exchange
func New(ctx log.Interface) *Exchange {
	return &Exchange{
		ctx:            ctx,
		done:           make(chan struct{}),
		northboundDone: make(map[string][]chan struct{}),
		southboundDone: make(map[string][]chan struct{}),
		connect:        make(chan *types.ConnectMessage),
		disconnect:     make(chan *types.DisconnectMessage),
		uplink:         make(chan *types.UplinkMessage),
		status:         make(chan *types.StatusMessage),
		downlink:       make(chan *types.DownlinkMessage),
	}
}

// AddNorthbound adds a new northbound backend (server that is up the chain)
func (b *Exchange) AddNorthbound(backend ...backend.Northbound) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.northboundBackends = append(b.northboundBackends, backend...)
}

func (b *Exchange) subscribeNorthbound(backend backend.Northbound) {
	if err := backend.Connect(); err != nil {
		b.ctx.WithError(err).Errorf("Could not set up backend %v", backend)
	}
	for {
		select {
		case <-b.done:
			return
		}
	}
}

// AddSouthbound adds a new southbound backend (gateway or server that is down the chain)
func (b *Exchange) AddSouthbound(backend ...backend.Southbound) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.southboundBackends = append(b.southboundBackends, backend...)
}

func (b *Exchange) subscribeSouthbound(backend backend.Southbound) {
	if err := backend.Connect(); err != nil {
		b.ctx.WithError(err).Errorf("Could not set up backend %v", backend)
	}
	connect, err := backend.SubscribeConnect()
	if err != nil {
		b.ctx.WithError(err).Errorf("Could not subscribe to connect from backend %v", backend)
	}
	disconnect, err := backend.SubscribeDisconnect()
	if err != nil {
		b.ctx.WithError(err).Errorf("Could not subscribe to disconnect from backend %v", backend)
	}
loop:
	for {
		select {
		case <-b.done:
			break loop
		case connectMessage := <-connect:
			b.connect <- connectMessage
		case disconnectMessage := <-disconnect:
			b.disconnect <- disconnectMessage
		}
	}
	if err := backend.UnsubscribeConnect(); err != nil {
		b.ctx.WithError(err).Errorf("Could not unsubscribe from connect on backend %v", backend)
	}
	if err := backend.UnsubscribeDisconnect(); err != nil {
		b.ctx.WithError(err).Errorf("Could not unsubscribe from disconnect on backend %v", backend)
	}
}

func (b *Exchange) handleChannels() {
	for {
		select {
		case <-b.done:
			return
		case connectMessage, ok := <-b.connect:
			if !ok {
				continue
			}
			for _, backend := range b.northboundBackends {
				go b.activateNorthbound(backend, connectMessage.GatewayID)
			}
			for _, backend := range b.southboundBackends {
				go b.activateSouthbound(backend, connectMessage.GatewayID)
			}
		case disconnectMessage, ok := <-b.disconnect:
			if !ok {
				continue
			}
			b.deactivateNorthbound(disconnectMessage.GatewayID)
			b.deactivateSouthbound(disconnectMessage.GatewayID)
		case uplinkMessage, ok := <-b.uplink:
			if !ok {
				continue
			}
			for _, backend := range b.northboundBackends {
				if err := backend.PublishUplink(uplinkMessage); err != nil {
					b.ctx.WithFields(log.Fields{
						"Backend":   fmt.Sprintf("%T", backend),
						"GatewayID": uplinkMessage.GatewayID,
					}).WithError(err).Warn("Could not publish uplink")
				}
			}
		case downlinkMessage, ok := <-b.downlink:
			if !ok {
				continue
			}
			for _, backend := range b.southboundBackends {
				if err := backend.PublishDownlink(downlinkMessage); err != nil {
					b.ctx.WithFields(log.Fields{
						"Backend":   fmt.Sprintf("%T", backend),
						"GatewayID": downlinkMessage.GatewayID,
					}).WithError(err).Warn("Could not publish downlink")
				}
			}
		case statusMessage, ok := <-b.status:
			if !ok {
				continue
			}
			for _, backend := range b.northboundBackends {
				if err := backend.PublishStatus(statusMessage); err != nil {
					b.ctx.WithFields(log.Fields{
						"Backend":   fmt.Sprintf("%T", backend),
						"GatewayID": statusMessage.GatewayID,
					}).WithError(err).Warn("Could not publish status")
				}
			}
		}
	}
}

func (b *Exchange) activateNorthbound(backend backend.Northbound, gatewayID string) {
	ctx := b.ctx.WithField("GatewayID", gatewayID).WithField("Backend", fmt.Sprintf("%T", backend))
	downlink, err := backend.SubscribeDownlink(gatewayID)
	if err != nil {
		ctx.WithError(err).Error("Could not subscribe to downlink")
	}
	done := make(chan struct{})
	b.doneLock.Lock()
	b.northboundDone[gatewayID] = append(b.northboundDone[gatewayID], done)
	b.doneLock.Unlock()
	ctx.Debug("Activated southbound")
loop:
	for {
		select {
		case <-done:
			break loop
		case downlinkMessage, ok := <-downlink:
			if !ok {
				continue
			}
			b.downlink <- downlinkMessage
		}
	}
	if err := backend.UnsubscribeDownlink(gatewayID); err != nil {
		ctx.WithError(err).Error("Could not unsubscribe from downlink")
	}
	ctx.Debug("Deactivated southbound")
}

func (b *Exchange) activateSouthbound(backend backend.Southbound, gatewayID string) {
	ctx := b.ctx.WithField("GatewayID", gatewayID).WithField("Backend", fmt.Sprintf("%T", backend))
	uplink, err := backend.SubscribeUplink(gatewayID)
	if err != nil {
		ctx.WithError(err).Error("Could not subscribe to uplink")
	}
	status, err := backend.SubscribeStatus(gatewayID)
	if err != nil {
		ctx.WithError(err).Error("Could not subscribe to status")
	}
	done := make(chan struct{})
	b.doneLock.Lock()
	b.southboundDone[gatewayID] = append(b.southboundDone[gatewayID], done)
	b.doneLock.Unlock()
	ctx.Debug("Activated southbound")
loop:
	for {
		select {
		case <-done:
			break loop
		case uplinkMessage, ok := <-uplink:
			if !ok {
				continue
			}
			b.uplink <- uplinkMessage
		case statusMessage, ok := <-status:
			if !ok {
				continue
			}
			b.status <- statusMessage
		}
	}
	if err := backend.UnsubscribeUplink(gatewayID); err != nil {
		ctx.WithError(err).Error("Could not unsubscribe from uplink")
	}
	if err := backend.UnsubscribeStatus(gatewayID); err != nil {
		ctx.WithError(err).Error("Could not unsubscribe from status")
	}
	ctx.Debug("Deactivated southbound")
}

func (b *Exchange) deactivateNorthbound(gatewayID string) {
	b.doneLock.Lock()
	defer b.doneLock.Unlock()
	if backends, ok := b.northboundDone[gatewayID]; ok {
		for _, done := range backends {
			close(done)
		}
		delete(b.northboundDone, gatewayID)
	}
}

func (b *Exchange) deactivateSouthbound(gatewayID string) {
	b.doneLock.Lock()
	defer b.doneLock.Unlock()
	if backends, ok := b.southboundDone[gatewayID]; ok {
		for _, done := range backends {
			close(done)
		}
		delete(b.southboundDone, gatewayID)
	}
}

// Start the Exchange
func (b *Exchange) Start() {
	b.mu.Lock()
	for _, backend := range b.northboundBackends {
		go b.subscribeNorthbound(backend)
	}
	for _, backend := range b.southboundBackends {
		go b.subscribeSouthbound(backend)
	}
	go b.handleChannels()
}

// Stop the Exchange
func (b *Exchange) Stop() {
	close(b.done) // This stops all new connections/disconnections
	b.doneLock.Lock()
	defer b.doneLock.Unlock()
	for _, backends := range b.northboundDone {
		for _, backend := range backends {
			close(backend)
		}
	}
	for _, backends := range b.southboundDone {
		for _, backend := range backends {
			close(backend)
		}
	}
	b.northboundDone = make(map[string][]chan struct{})
	b.southboundDone = make(map[string][]chan struct{})
	b.mu.Unlock()
}