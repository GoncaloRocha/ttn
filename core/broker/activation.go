// Copyright © 2016 The Things Network
// Use of this source code is governed by the MIT license that can be found in the LICENSE file.

package broker

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	pb "github.com/TheThingsNetwork/ttn/api/broker"
	pb_discovery "github.com/TheThingsNetwork/ttn/api/discovery"
	"github.com/TheThingsNetwork/ttn/api/gateway"
	pb_handler "github.com/TheThingsNetwork/ttn/api/handler"
	"github.com/TheThingsNetwork/ttn/utils/errors"
	"github.com/apex/log"
	"github.com/brocaar/lorawan"
)

type challengeResponseWithHandler struct {
	handler  *pb_discovery.Announcement
	client   pb_handler.HandlerClient
	response *pb.ActivationChallengeResponse
}

func (b *broker) HandleActivation(activation *pb.DeviceActivationRequest) (*pb.DeviceActivationResponse, error) {
	ctx := b.Ctx.WithFields(log.Fields{
		"GatewayID": activation.GatewayMetadata.GatewayId,
		"AppEUI":    *activation.AppEui,
		"DevEUI":    *activation.DevEui,
	})
	var err error
	start := time.Now()
	defer func() {
		if err != nil {
			ctx.WithError(err).Warn("Could not handle activation")
		} else {
			ctx.WithField("Duration", time.Now().Sub(start)).Info("Handled activation")
		}
	}()

	time := time.Now()

	// De-duplicate uplink messages
	duplicates := b.deduplicateActivation(activation)
	if len(duplicates) == 0 {
		err = errors.NewErrInternal("No duplicates")
		return nil, err
	}

	base := duplicates[0]

	// Collect GatewayMetadata and DownlinkOptions
	var gatewayMetadata []*gateway.RxMetadata
	var downlinkOptions []*pb.DownlinkOption
	var deviceActivationResponse *pb.DeviceActivationResponse
	for _, duplicate := range duplicates {
		gatewayMetadata = append(gatewayMetadata, duplicate.GatewayMetadata)
		downlinkOptions = append(downlinkOptions, duplicate.DownlinkOptions...)
	}

	// Select best DownlinkOption
	if len(downlinkOptions) > 0 {
		deviceActivationResponse = &pb.DeviceActivationResponse{
			DownlinkOption: selectBestDownlink(downlinkOptions),
		}
	}

	// Build Uplink
	deduplicatedActivationRequest := &pb.DeduplicatedDeviceActivationRequest{
		Payload:            base.Payload,
		DevEui:             base.DevEui,
		AppEui:             base.AppEui,
		ProtocolMetadata:   base.ProtocolMetadata,
		GatewayMetadata:    gatewayMetadata,
		ActivationMetadata: base.ActivationMetadata,
		ServerTime:         time.UnixNano(),
		ResponseTemplate:   deviceActivationResponse,
	}

	// Send Activate to NS
	deduplicatedActivationRequest, err = b.ns.PrepareActivation(b.Component.GetContext(b.nsToken), deduplicatedActivationRequest)
	if err != nil {
		err = errors.Wrap(errors.FromGRPCError(err), "NetworkServer refused to prepare activation")
		return nil, err
	}

	ctx = ctx.WithFields(log.Fields{
		"AppID": deduplicatedActivationRequest.AppId,
		"DevID": deduplicatedActivationRequest.DevId,
	})

	// Find Handler (based on AppEUI)
	var announcements []*pb_discovery.Announcement
	announcements, err = b.Discovery.GetAllHandlersForAppID(deduplicatedActivationRequest.AppId)
	if err != nil {
		return nil, err
	}
	if len(announcements) == 0 {
		err = errors.NewErrNotFound(fmt.Sprintf("Handler for AppID %s", deduplicatedActivationRequest.AppId))
		return nil, err
	}

	ctx = ctx.WithField("NumHandlers", len(announcements))

	// LoRaWAN: Unmarshal and prepare version without MIC
	var phyPayload lorawan.PHYPayload
	err = phyPayload.UnmarshalBinary(deduplicatedActivationRequest.Payload)
	if err != nil {
		return nil, err
	}
	correctMIC := phyPayload.MIC
	phyPayload.MIC = [4]byte{0, 0, 0, 0}
	phyPayloadWithoutMIC, err := phyPayload.MarshalBinary()
	if err != nil {
		return nil, err
	}

	// Build Challenge
	challenge := &pb.ActivationChallengeRequest{
		Payload: phyPayloadWithoutMIC,
		AppId:   deduplicatedActivationRequest.AppId,
		DevId:   deduplicatedActivationRequest.DevId,
		AppEui:  deduplicatedActivationRequest.AppEui,
		DevEui:  deduplicatedActivationRequest.DevEui,
	}

	// Send Challenge to all handlers and collect responses
	var wg sync.WaitGroup
	responses := make(chan *challengeResponseWithHandler, len(announcements))
	for _, announcement := range announcements {
		conn, err := announcement.Dial()
		if err != nil {
			ctx.WithError(err).Warn("Could not dial handler for Activation")
			continue
		}
		client := pb_handler.NewHandlerClient(conn)

		// Do async request
		wg.Add(1)
		go func(announcement *pb_discovery.Announcement) {
			res, err := client.ActivationChallenge(b.Component.GetContext(""), challenge)
			if err == nil && res != nil {
				responses <- &challengeResponseWithHandler{
					handler:  announcement,
					client:   client,
					response: res,
				}
			}
			wg.Done()
		}(announcement)
	}

	// Make sure to close channel when all requests are done
	go func() {
		wg.Wait()
		close(responses)
	}()

	var gotFirst bool
	var joinHandler *pb_discovery.Announcement
	var joinHandlerClient pb_handler.HandlerClient
	for res := range responses {
		var phyPayload lorawan.PHYPayload
		err = phyPayload.UnmarshalBinary(res.response.Payload)
		if err != nil {
			continue
		}
		if phyPayload.MIC != correctMIC {
			continue
		}

		if gotFirst {
			ctx.Warn("Duplicate Activation Response")
		} else {
			gotFirst = true
			joinHandler = res.handler
			joinHandlerClient = res.client
		}
	}

	// Activation not accepted by any broker
	if !gotFirst {
		ctx.Debug("Activation not accepted by any Handler")
		err = errors.New("Activation not accepted by any Handler")
		return nil, err
	}

	ctx.WithField("HandlerID", joinHandler.Id).Debug("Forward Activation")

	var handlerResponse *pb_handler.DeviceActivationResponse
	handlerResponse, err = joinHandlerClient.Activate(b.Component.GetContext(""), deduplicatedActivationRequest)
	if err != nil {
		err = errors.Wrap(errors.FromGRPCError(err), "Handler refused activation")
		return nil, err
	}

	handlerResponse, err = b.ns.Activate(b.Component.GetContext(b.nsToken), handlerResponse)
	if err != nil {
		err = errors.Wrap(errors.FromGRPCError(err), "NetworkServer refused activation")
		return nil, err
	}

	deviceActivationResponse = &pb.DeviceActivationResponse{
		Payload:        handlerResponse.Payload,
		DownlinkOption: handlerResponse.DownlinkOption,
	}

	return deviceActivationResponse, nil
}

func (b *broker) deduplicateActivation(duplicate *pb.DeviceActivationRequest) (activations []*pb.DeviceActivationRequest) {
	sum := md5.Sum(duplicate.Payload)
	key := hex.EncodeToString(sum[:])
	list := b.activationDeduplicator.Deduplicate(key, duplicate)
	if len(list) == 0 {
		return
	}
	for _, duplicate := range list {
		activations = append(activations, duplicate.(*pb.DeviceActivationRequest))
	}
	return
}
