// Copyright © 2016 The Things Network
// Use of this source code is governed by the MIT license that can be found in the LICENSE file.

package handler

import (
	"fmt"
	"time"

	pb_broker "github.com/TheThingsNetwork/ttn/api/broker"
	pb "github.com/TheThingsNetwork/ttn/api/handler"
	"github.com/TheThingsNetwork/ttn/core"
	"github.com/TheThingsNetwork/ttn/core/handler/device"
	"github.com/TheThingsNetwork/ttn/core/otaa"
	"github.com/TheThingsNetwork/ttn/core/types"
	"github.com/TheThingsNetwork/ttn/mqtt"
	"github.com/TheThingsNetwork/ttn/utils/random"
	"github.com/apex/log"
	"github.com/brocaar/lorawan"
)

func (h *handler) getActivationMetadata(ctx log.Interface, activation *pb_broker.DeduplicatedDeviceActivationRequest) (mqtt.Metadata, error) {
	ttnUp := &pb_broker.DeduplicatedUplinkMessage{
		ProtocolMetadata: activation.ProtocolMetadata,
		GatewayMetadata:  activation.GatewayMetadata,
		ServerTime:       activation.ServerTime,
	}
	mqttUp := &mqtt.UplinkMessage{}
	err := h.ConvertMetadata(ctx, ttnUp, mqttUp)
	if err != nil {
		return mqtt.Metadata{}, err
	}
	return mqttUp.Metadata, nil
}

func (h *handler) HandleActivationChallenge(challenge *pb_broker.ActivationChallengeRequest) (*pb_broker.ActivationChallengeResponse, error) {

	// Find Device
	dev, err := h.devices.Get(challenge.AppId, challenge.DevId)
	if err != nil {
		return nil, err
	}

	if dev.AppKey.IsEmpty() {
		err = core.NewErrNotFound(fmt.Sprintf("AppKey for device %s", challenge.DevId))
		return nil, err
	}

	// Unmarshal LoRaWAN
	var reqPHY lorawan.PHYPayload
	if err = reqPHY.UnmarshalBinary(challenge.Payload); err != nil {
		return nil, err
	}

	// Set MIC
	if err := reqPHY.SetMIC(lorawan.AES128Key(dev.AppKey)); err != nil {
		err = core.NewErrNotFound("Could not set MIC")
		return nil, err
	}

	// Marshal
	bytes, err := reqPHY.MarshalBinary()
	if err != nil {
		return nil, err
	}

	return &pb_broker.ActivationChallengeResponse{
		Payload: bytes,
	}, nil
}

func (h *handler) HandleActivation(activation *pb_broker.DeduplicatedDeviceActivationRequest) (*pb.DeviceActivationResponse, error) {
	var appEUI types.AppEUI
	if activation.AppEui != nil {
		appEUI = *activation.AppEui
	}
	var devEUI types.DevEUI
	if activation.DevEui != nil {
		devEUI = *activation.DevEui
	}
	ctx := h.Ctx.WithFields(log.Fields{
		"DevEUI": devEUI,
		"AppEUI": appEUI,
		"AppID":  activation.AppId,
		"DevID":  activation.DevId,
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

	if activation.ResponseTemplate == nil {
		err = core.NewErrInternal("No downlink available")
		return nil, err
	}

	// Find Device
	var dev *device.Device
	dev, err = h.devices.Get(activation.AppId, activation.DevId)
	if err != nil {
		return nil, err
	}

	if dev.AppKey.IsEmpty() {
		err = core.NewErrNotFound(fmt.Sprintf("AppKey for device %s", activation.DevId))
		return nil, err
	}

	// Check for LoRaWAN
	if lorawan := activation.ActivationMetadata.GetLorawan(); lorawan == nil {
		err = core.NewErrInvalidArgument("Activation", "does not contain LoRaWAN metadata")
		return nil, err
	}

	// Unmarshal LoRaWAN
	var reqPHY lorawan.PHYPayload
	if err = reqPHY.UnmarshalBinary(activation.Payload); err != nil {
		return nil, err
	}
	reqMAC, ok := reqPHY.MACPayload.(*lorawan.JoinRequestPayload)
	if !ok {
		err = core.NewErrInvalidArgument("Activation", "does not contain a JoinRequestPayload")
		return nil, err
	}

	// Validate MIC
	if ok, err = reqPHY.ValidateMIC(lorawan.AES128Key(dev.AppKey)); err != nil || !ok {
		err = core.NewErrNotFound("MIC does not match device")
		return nil, err
	}

	// Validate DevNonce
	var alreadyUsed bool
	for _, usedNonce := range dev.UsedDevNonces {
		if usedNonce == device.DevNonce(reqMAC.DevNonce) {
			alreadyUsed = true
			break
		}
	}
	if alreadyUsed {
		err = core.NewErrInvalidArgument("Activation DevNonce", "already used")
		return nil, err
	}

	ctx.Debug("Accepting Join Request")

	// Prepare Device Activation Response
	var resPHY lorawan.PHYPayload
	if err = resPHY.UnmarshalBinary(activation.ResponseTemplate.Payload); err != nil {
		return nil, err
	}
	resMAC, ok := resPHY.MACPayload.(*lorawan.DataPayload)
	if !ok {
		err = core.NewErrInvalidArgument("Activation ResponseTemplate", "MACPayload must be a *DataPayload")
		return nil, err
	}
	joinAccept := &lorawan.JoinAcceptPayload{}
	if err = joinAccept.UnmarshalBinary(false, resMAC.Bytes); err != nil {
		return nil, err
	}
	resPHY.MACPayload = joinAccept

	// Publish Activation
	mqttMetadata, _ := h.getActivationMetadata(ctx, activation)
	h.mqttActivation <- &mqtt.Activation{
		AppEUI:   *activation.AppEui,
		DevEUI:   *activation.DevEui,
		AppID:    activation.AppId,
		DevID:    activation.DevId,
		DevAddr:  types.DevAddr(joinAccept.DevAddr),
		Metadata: mqttMetadata,
	}

	// Generate random AppNonce
	var appNonce device.AppNonce
	for {
		// NOTE: As DevNonces are only 2 bytes, we will start rejecting those before we run out of AppNonces.
		// It might just take some time to get one we didn't use yet...
		alreadyUsed = false
		copy(appNonce[:], random.New().Bytes(3))
		for _, usedNonce := range dev.UsedAppNonces {
			if usedNonce == appNonce {
				alreadyUsed = true
				break
			}
		}
		if !alreadyUsed {
			break
		}
	}
	joinAccept.AppNonce = appNonce

	// Calculate session keys
	var appSKey types.AppSKey
	var nwkSKey types.NwkSKey
	appSKey, nwkSKey, err = otaa.CalculateSessionKeys(dev.AppKey, joinAccept.AppNonce, joinAccept.NetID, reqMAC.DevNonce)
	if err != nil {
		return nil, err
	}

	// Update Device
	dev.DevAddr = types.DevAddr(joinAccept.DevAddr)
	dev.AppSKey = appSKey
	dev.NwkSKey = nwkSKey
	dev.UsedAppNonces = append(dev.UsedAppNonces, appNonce)
	dev.UsedDevNonces = append(dev.UsedDevNonces, reqMAC.DevNonce)
	err = h.devices.Set(dev, "dev_addr", "app_key", "app_s_key", "nwk_s_key", "used_app_nonces", "used_dev_nonces") // app_key is only needed when the default app_key is used to activate the device
	if err != nil {
		return nil, err
	}

	if err = resPHY.SetMIC(lorawan.AES128Key(dev.AppKey)); err != nil {
		return nil, err
	}
	if err = resPHY.EncryptJoinAcceptPayload(lorawan.AES128Key(dev.AppKey)); err != nil {
		return nil, err
	}

	var resBytes []byte
	resBytes, err = resPHY.MarshalBinary()
	if err != nil {
		return nil, err
	}

	metadata := activation.ActivationMetadata
	metadata.GetLorawan().NwkSKey = &dev.NwkSKey
	metadata.GetLorawan().DevAddr = &dev.DevAddr
	res := &pb.DeviceActivationResponse{
		Payload:            resBytes,
		DownlinkOption:     activation.ResponseTemplate.DownlinkOption,
		ActivationMetadata: metadata,
		AppId:              dev.AppID,
	}

	return res, nil
}