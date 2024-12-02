// Copyright 2023 LiveKit, Inc.
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

package remotebwe

import (
	"sync"
	"time"

	"github.com/livekit/livekit-server/pkg/sfu/bwe"
	"github.com/livekit/livekit-server/pkg/sfu/ccutils"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils/mono"
)

// ---------------------------------------------------------------------------

type RemoteBWEConfig struct {
	NackRatioAttenuator     float64               `yaml:"nack_ratio_attenuator,omitempty"`
	ExpectedUsageThreshold  float64               `yaml:"expected_usage_threshold,omitempty"`
	ChannelObserverProbe    ChannelObserverConfig `yaml:"channel_observer_probe,omitempty"`
	ChannelObserverNonProbe ChannelObserverConfig `yaml:"channel_observer_non_probe,omitempty"`
	CongestedMinDuration    time.Duration         `yaml:"congested_min_duration,omitempty"`

	PeriodicCheckInterval          time.Duration `yaml:"periodic_check_interval,omitempty"`
	PeriodicCheckIntervalCongested time.Duration `yaml:"periodic_check_interval_congested,omitempty"`
}

var (
	DefaultRemoteBWEConfig = RemoteBWEConfig{
		NackRatioAttenuator:            0.4,
		ExpectedUsageThreshold:         0.95,
		ChannelObserverProbe:           defaultChannelObserverConfigProbe,
		ChannelObserverNonProbe:        defaultChannelObserverConfigNonProbe,
		CongestedMinDuration:           3 * time.Second,
		PeriodicCheckInterval:          2 * time.Second,
		PeriodicCheckIntervalCongested: 200 * time.Millisecond,
	}
)

// ---------------------------------------------------------------------------

type RemoteBWEParams struct {
	Config RemoteBWEConfig
	Logger logger.Logger
}

type RemoteBWE struct {
	bwe.NullBWE

	params RemoteBWEParams

	lock sync.RWMutex

	lastReceivedEstimate       int64
	lastExpectedBandwidthUsage int64
	committedChannelCapacity   int64

	isInProbe       bool
	channelObserver *channelObserver

	congestionState           bwe.CongestionState
	congestionStateSwitchedAt time.Time

	bweListener bwe.BWEListener
}

func NewRemoteBWE(params RemoteBWEParams) *RemoteBWE {
	r := &RemoteBWE{
		params: params,
	}

	r.Reset()
	return r
}

func (r *RemoteBWE) SetBWEListener(bweListener bwe.BWEListener) {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.bweListener = bweListener
}

func (r *RemoteBWE) getBWEListener() bwe.BWEListener {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.bweListener
}

func (r *RemoteBWE) Reset() {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.lastReceivedEstimate = 0
	r.lastExpectedBandwidthUsage = 0
	r.committedChannelCapacity = 100_000_000

	r.isInProbe = false
	r.newChannelObserver()

	r.congestionState = bwe.CongestionStateNone
	r.congestionStateSwitchedAt = mono.Now()
}

func (r *RemoteBWE) HandleREMB(
	receivedEstimate int64,
	expectedBandwidthUsage int64,
	sentPackets uint32,
	repeatedNacks uint32,
) {
	r.lock.Lock()
	r.lastReceivedEstimate = receivedEstimate
	r.lastExpectedBandwidthUsage = expectedBandwidthUsage

	// in probe, freeze channel observer state if probe causes congestion till the probe is done,
	// this is to ensure that probe result is not a success and an unsuccessful probe will not up allocate any tracks
	if r.isInProbe && r.congestionState != bwe.CongestionStateNone {
		r.lock.Unlock()
		return
	}

	r.channelObserver.AddEstimate(r.lastReceivedEstimate)
	r.channelObserver.AddNack(sentPackets, repeatedNacks)

	shouldNotify, state, committedChannelCapacity := r.congestionDetectionStateMachine()
	r.lock.Unlock()

	if shouldNotify {
		if bweListener := r.getBWEListener(); bweListener != nil {
			bweListener.OnCongestionStateChange(state, committedChannelCapacity)
		}
	}
}

func (r *RemoteBWE) CongestionState() bwe.CongestionState {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.congestionState
}

func (r *RemoteBWE) congestionDetectionStateMachine() (bool, bwe.CongestionState, int64) {
	newState := r.congestionState
	update := false
	trend, reason := r.channelObserver.GetTrend()
	if trend == channelTrendCongesting {
		r.params.Logger.Debugw("remote bwe, channel congesting", "channel", r.channelObserver)
	}

	switch r.congestionState {
	case bwe.CongestionStateNone:
		if trend == channelTrendCongesting {
			if r.isInProbe || r.estimateAvailableChannelCapacity(reason) {
				// when in probe, if congested, stays there will probe is done,
				// the estimate stays at pre-probe level
				newState = bwe.CongestionStateCongested
			}
		}

	case bwe.CongestionStateCongested:
		if trend == channelTrendCongesting {
			if r.estimateAvailableChannelCapacity(reason) {
				// update state as this needs to reset switch time to wait for congestion min duration again
				update = true
			}
		} else {
			newState = bwe.CongestionStateCongestedHangover
		}

	case bwe.CongestionStateCongestedHangover:
		if trend == channelTrendCongesting {
			if r.estimateAvailableChannelCapacity(reason) {
				newState = bwe.CongestionStateCongested
			}
		} else if time.Since(r.congestionStateSwitchedAt) >= r.params.Config.CongestedMinDuration {
			newState = bwe.CongestionStateNone
		}
	}

	shouldNotify := false
	if newState != r.congestionState || update {
		r.updateCongestionState(newState, reason)
		shouldNotify = true
	}

	return shouldNotify, r.congestionState, r.committedChannelCapacity
}

func (r *RemoteBWE) estimateAvailableChannelCapacity(reason channelCongestionReason) bool {
	var estimateToCommit int64
	switch reason {
	case channelCongestionReasonLoss:
		estimateToCommit = int64(float64(r.lastExpectedBandwidthUsage) * (1.0 - r.params.Config.NackRatioAttenuator*r.channelObserver.GetNackRatio()))
	default:
		estimateToCommit = r.lastReceivedEstimate
	}
	if estimateToCommit > r.lastReceivedEstimate {
		estimateToCommit = r.lastReceivedEstimate
	}

	commitThreshold := int64(r.params.Config.ExpectedUsageThreshold * float64(r.lastExpectedBandwidthUsage))

	ulgr := r.params.Logger.WithUnlikelyValues(
		"reason", reason,
		"old(bps)", r.committedChannelCapacity,
		"new(bps)", estimateToCommit,
		"lastReceived(bps)", r.lastReceivedEstimate,
		"expectedUsage(bps)", r.lastExpectedBandwidthUsage,
		"commitThreshold(bps)", commitThreshold,
		"channel", r.channelObserver,
	)
	if estimateToCommit > commitThreshold {
		ulgr.Debugw("remote bwe: channel congestion detected, skipping above commit threshold channel capacity update")
		return false
	}

	ulgr.Infow("remote bwe: channel congestion detected, applying channel capacity update")
	/* REMOTE-BWE-DATA
	r.params.Logger.Debugw(
		fmt.Sprintf("remote bwe: channel congestion detected, %s channel capacity: experimental", action),
		"nackHistory", r.channelObserver.GetNackHistory(),
	)
	*/

	r.committedChannelCapacity = estimateToCommit

	// reset to get new set of samples for next trend
	r.newChannelObserver()
	return true
}

func (r *RemoteBWE) updateCongestionState(state bwe.CongestionState, reason channelCongestionReason) {
	r.params.Logger.Infow(
		"remote bwe: congestion state change",
		"from", r.congestionState,
		"to", state,
		"reason", reason,
		"committedChannelCapacity", r.committedChannelCapacity,
	)

	r.congestionState = state
	r.congestionStateSwitchedAt = mono.Now()
}

func (r *RemoteBWE) ProbeClusterStarting(pci ccutils.ProbeClusterInfo) {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.lastExpectedBandwidthUsage = int64(pci.Goal.ExpectedUsageBps)

	r.params.Logger.Debugw(
		"remote bwe: starting probe",
		"lastReceived", r.lastReceivedEstimate,
		"expectedBandwidthUsage", r.lastExpectedBandwidthUsage,
		"channel", r.channelObserver,
	)

	r.isInProbe = true
	r.newChannelObserver()
}

func (r *RemoteBWE) ProbeClusterDone(_pci ccutils.ProbeClusterInfo) (bwe.ProbeSignal, int64) {
	r.lock.Lock()
	defer r.lock.Unlock()

	// switch to a non-probe channel observer on probe end,
	// reset congestion state to get a fresh trend
	pco := r.channelObserver
	probeCongestionState := r.congestionState

	r.isInProbe = false
	r.congestionState = bwe.CongestionStateNone
	r.newChannelObserver()

	r.params.Logger.Debugw(
		"remote bwe: probe done",
		"lastReceived", r.lastReceivedEstimate,
		"expectedBandwidthUsage", r.lastExpectedBandwidthUsage,
		"channel", pco,
		"isSignalValid", pco.HasEnoughEstimateSamples(),
	)

	if probeCongestionState != bwe.CongestionStateNone {
		return bwe.ProbeSignalCongesting, r.committedChannelCapacity
	}

	trend, _ := pco.GetTrend()
	if !pco.HasEnoughEstimateSamples() || trend == channelTrendNeutral {
		return bwe.ProbeSignalInconclusive, r.committedChannelCapacity
	}

	highestEstimate := pco.GetHighestEstimate()
	if highestEstimate > r.committedChannelCapacity {
		r.committedChannelCapacity = highestEstimate
	}
	return bwe.ProbeSignalClearing, r.committedChannelCapacity
}

func (r *RemoteBWE) newChannelObserver() {
	if r.isInProbe {
		r.channelObserver = newChannelObserver(
			channelObserverParams{
				Name:   "probe",
				Config: r.params.Config.ChannelObserverProbe,
			},
			r.params.Logger,
		)
		r.channelObserver.SeedEstimate(r.committedChannelCapacity)
	} else {
		r.channelObserver = newChannelObserver(
			channelObserverParams{
				Name:   "non-probe",
				Config: r.params.Config.ChannelObserverNonProbe,
			},
			r.params.Logger,
		)
	}
}
