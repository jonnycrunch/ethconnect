// Copyright 2019 Kaleido

// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at

//     http://www.apache.org/licenses/LICENSE-2.0

// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kldevents

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/kaleido-io/ethconnect/internal/kldbind"
	"github.com/kaleido-io/ethconnect/internal/kldeth"
	log "github.com/sirupsen/logrus"
)

// persistedFilter is the part of the filter we record to storage
type persistedFilter struct {
	Addresses []kldbind.Address `json:"address,omitempty"`
	Topics    [][]kldbind.Hash  `json:"topics,omitempty"`
}

// ethFilterInitial is the filter structure we send over the wire on eth_newFilter when we first register
type ethFilterInitial struct {
	persistedFilter
	FromBlock string `json:"fromBlock,omitempty"`
	ToBlock   string `json:"toBlock,omitempty"`
}

// ethFilterRestart is the filter structure we send over the wire on eth_newFilter when we restart
type ethFilterRestart struct {
	persistedFilter
	FromBlock kldbind.HexBigInt `json:"fromBlock,omitempty"`
	ToBlock   string            `json:"toBlock,omitempty"`
}

// SubscriptionInfo is the persisted data for the subscription
type SubscriptionInfo struct {
	ID             string                     `json:"id,omitempty"`
	Path           string                     `json:"path"`
	CreatedISO8601 string                     `json:"created"`
	Name           string                     `json:"name"`
	Stream         string                     `json:"stream"`
	Filter         persistedFilter            `json:"filter"`
	Event          kldbind.MarshalledABIEvent `json:"event"`
}

// subscription is the runtime that manages the subscription
type subscription struct {
	info         *SubscriptionInfo
	rpc          kldeth.RPCClient
	lp           *logProcessor
	logName      string
	filterID     kldbind.HexBigInt
	filteredOnce bool
	filterStale  bool
}

func newSubscription(sm subscriptionManager, rpc kldeth.RPCClient, addr *kldbind.Address, i *SubscriptionInfo) (*subscription, error) {
	stream, err := sm.streamByID(i.Stream)
	if err != nil {
		return nil, err
	}
	s := &subscription{
		info:        i,
		rpc:         rpc,
		lp:          newLogProcessor(i.ID, &i.Event.E, stream),
		logName:     i.ID + ":" + eventSummary(&i.Event.E),
		filterStale: true,
	}
	f := &i.Filter
	addrStr := "*"
	if addr != nil {
		f.Addresses = []kldbind.Address{*addr}
		addrStr = addr.String()
	}
	event := &i.Event.E
	i.Name = addrStr + ":" + eventSummary(event)
	if event == nil || event.Name == "" {
		return nil, fmt.Errorf("Solidity event name must be specified")
	}
	// For now we only support filtering on the event type
	f.Topics = [][]kldbind.Hash{[]kldbind.Hash{event.Id()}}
	log.Infof("Created subscription %s %s topic:%s", i.ID, i.Name, event.Id().String())
	return s, nil
}

func eventSummary(e *kldbind.ABIEvent) string {
	var sb strings.Builder
	sb.WriteString(e.Name)
	sb.WriteString("(")
	for idx, input := range e.Inputs {
		if idx > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(input.Type.String())
	}
	sb.WriteString(")")
	return sb.String()
}

func restoreSubscription(sm subscriptionManager, rpc kldeth.RPCClient, i *SubscriptionInfo) (*subscription, error) {
	if i.ID == "" {
		return nil, fmt.Errorf("No ID")
	}
	stream, err := sm.streamByID(i.Stream)
	if err != nil {
		return nil, err
	}
	s := &subscription{
		rpc:         rpc,
		info:        i,
		lp:          newLogProcessor(i.ID, &i.Event.E, stream),
		logName:     i.ID + ":" + eventSummary(&i.Event.E),
		filterStale: true,
	}
	return s, nil
}

func (s *subscription) initialFilter() error {
	f := &ethFilterInitial{}
	f.persistedFilter = s.info.Filter
	f.FromBlock = "latest"
	f.ToBlock = "latest"

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := s.rpc.CallContext(ctx, &s.filterID, "eth_newFilter", f)
	if err != nil {
		return err
	}
	log.Infof("%s: created initial filter: %s", s.logName, s.filterID.String())
	s.filteredOnce = false
	s.filterStale = false
	return nil
}

func (s *subscription) restartFilter(since *big.Int) error {
	f := &ethFilterRestart{}
	f.persistedFilter = s.info.Filter
	f.FromBlock.ToInt().Set(since)
	f.ToBlock = "latest"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := s.rpc.CallContext(ctx, &s.filterID, "eth_newFilter", f)
	if err != nil {
		return err
	}
	s.filteredOnce = false
	s.filterStale = false
	log.Infof("%s: created filter from block %s: %s", s.logName, since.String(), s.filterID.String())
	return err
}

func (s *subscription) processNewEvents() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var logs []*logEntry
	rpcMethod := "eth_getFilterLogs"
	if s.filteredOnce {
		rpcMethod = "eth_getFilterChanges"
	}
	if err := s.rpc.CallContext(ctx, &logs, rpcMethod, s.filterID); err != nil {
		if strings.Contains(err.Error(), "filter not found") {
			s.filterStale = true
		}
		return err
	}
	log.Infof("%s: received %d events (%s)", s.logName, len(logs), rpcMethod)
	for _, logEntry := range logs {
		if err := s.lp.processLogEntry(logEntry); err != nil {
			log.Errorf("Failed to processs event: %s", err)
		}
	}
	s.filteredOnce = true
	return nil
}

func (s *subscription) unsubscribe() error {
	var retval string
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s.filterStale = true
	err := s.rpc.CallContext(ctx, &retval, "eth_uninstallFilter", s.filterID)
	log.Infof("%s: Uninstalled filter (retval=%s)", s.logName, retval)
	return err
}

func (s *subscription) blockHWM() big.Int {
	return s.lp.getBlockHWM()
}
