// Copyright © 2016 The Things Network
// Use of this source code is governed by the MIT license that can be found in the LICENSE file.

package handler

import (
	"fmt"

	pb_broker "github.com/TheThingsNetwork/ttn/api/broker"
	pb_discovery "github.com/TheThingsNetwork/ttn/api/discovery"
	pb "github.com/TheThingsNetwork/ttn/api/handler"
	pb_lorawan "github.com/TheThingsNetwork/ttn/api/protocol/lorawan"
	"github.com/TheThingsNetwork/ttn/core"
	"github.com/TheThingsNetwork/ttn/core/handler/application"
	"github.com/TheThingsNetwork/ttn/core/handler/device"
	"github.com/golang/protobuf/ptypes/empty"
	"github.com/pkg/errors"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
)

type handlerManager struct {
	handler        *handler
	deviceManager  pb_lorawan.DeviceManagerClient
	devAddrManager pb_lorawan.DevAddrManagerClient
}

func (h *handlerManager) getDevice(ctx context.Context, in *pb.DeviceIdentifier) (*device.Device, error) {
	if !in.Validate() {
		return nil, core.NewErrInvalidArgument("Device Identifier", "validation failed")
	}
	claims, err := h.handler.Component.ValidateTTNAuthContext(ctx)
	if err != nil {
		return nil, err
	}
	if !claims.CanEditApp(in.AppId) {
		return nil, core.NewErrPermissionDenied(fmt.Sprintf("No access to Application %s", in.AppId))
	}
	dev, err := h.handler.devices.Get(in.AppId, in.DevId)
	if err != nil {
		return nil, err
	}
	if !claims.CanEditApp(dev.AppID) {
		return nil, core.NewErrPermissionDenied(fmt.Sprintf("No access to Application %s", in.AppId))
	}
	return dev, nil
}

func (h *handlerManager) GetDevice(ctx context.Context, in *pb.DeviceIdentifier) (*pb.Device, error) {
	dev, err := h.getDevice(ctx, in)
	if err != nil {
		return nil, core.BuildGRPCError(err)
	}

	nsDev, err := h.deviceManager.GetDevice(ctx, &pb_lorawan.DeviceIdentifier{
		AppEui: &dev.AppEUI,
		DevEui: &dev.DevEUI,
	})
	if err != nil {
		return nil, core.BuildGRPCError(errors.Wrap(core.FromGRPCError(err), "Broker did not return device"))
	}

	return &pb.Device{
		AppId: dev.AppID,
		DevId: dev.DevID,
		Device: &pb.Device_LorawanDevice{LorawanDevice: &pb_lorawan.Device{
			AppId:            dev.AppID,
			AppEui:           nsDev.AppEui,
			DevId:            dev.DevID,
			DevEui:           nsDev.DevEui,
			DevAddr:          nsDev.DevAddr,
			NwkSKey:          nsDev.NwkSKey,
			AppSKey:          &dev.AppSKey,
			AppKey:           &dev.AppKey,
			FCntUp:           nsDev.FCntUp,
			FCntDown:         nsDev.FCntDown,
			DisableFCntCheck: nsDev.DisableFCntCheck,
			Uses32BitFCnt:    nsDev.Uses32BitFCnt,
			LastSeen:         nsDev.LastSeen,
		}},
	}, nil
}

func (h *handlerManager) SetDevice(ctx context.Context, in *pb.Device) (*empty.Empty, error) {
	dev, err := h.getDevice(ctx, &pb.DeviceIdentifier{AppId: in.AppId, DevId: in.DevId})
	if err != nil && core.GetErrType(err) != core.NotFound {
		return nil, core.BuildGRPCError(err)
	}

	if !in.Validate() {
		return nil, grpcErrf(codes.InvalidArgument, "Invalid Device")
	}

	lorawan := in.GetLorawanDevice()
	if lorawan == nil {
		return nil, grpcErrf(codes.InvalidArgument, "No LoRaWAN Device")
	}

	if dev != nil { // When this is an update
		if dev.AppEUI != *lorawan.AppEui || dev.DevEUI != *lorawan.DevEui {
			// If the AppEUI or DevEUI is changed, we should remove the device from the NetworkServer and re-add it later
			_, err = h.deviceManager.DeleteDevice(ctx, &pb_lorawan.DeviceIdentifier{
				AppEui: &dev.AppEUI,
				DevEui: &dev.DevEUI,
			})
			if err != nil {
				return nil, core.BuildGRPCError(errors.Wrap(core.FromGRPCError(err), "Broker did not delete device"))
			}
		}
	} else { // When this is a create

	}

	updated := &device.Device{
		AppID:  in.AppId,
		DevID:  in.DevId,
		AppEUI: *lorawan.AppEui,
		DevEUI: *lorawan.DevEui,
	}

	if lorawan.DevAddr != nil {
		updated.DevAddr = *lorawan.DevAddr
	}
	if lorawan.NwkSKey != nil {
		updated.NwkSKey = *lorawan.NwkSKey
	}
	if lorawan.AppSKey != nil {
		updated.AppSKey = *lorawan.AppSKey
	}
	if lorawan.AppKey != nil {
		updated.AppKey = *lorawan.AppKey
	}

	nsUpdated := &pb_lorawan.Device{
		AppId:                 in.AppId,
		DevId:                 in.DevId,
		AppEui:                lorawan.AppEui,
		DevEui:                lorawan.DevEui,
		DevAddr:               lorawan.DevAddr,
		NwkSKey:               lorawan.NwkSKey,
		FCntUp:                lorawan.FCntUp,
		FCntDown:              lorawan.FCntDown,
		DisableFCntCheck:      lorawan.DisableFCntCheck,
		Uses32BitFCnt:         lorawan.Uses32BitFCnt,
		ActivationConstraints: lorawan.ActivationConstraints,
	}

	// Devices are activated locally by default
	if nsUpdated.ActivationConstraints == "" {
		nsUpdated.ActivationConstraints = "local"
	}

	_, err = h.deviceManager.SetDevice(ctx, nsUpdated)
	if err != nil {
		return nil, core.BuildGRPCError(errors.Wrap(core.FromGRPCError(err), "Broker did not set device"))
	}

	err = h.handler.devices.Set(updated)
	if err != nil {
		return nil, core.BuildGRPCError(err)
	}

	return &empty.Empty{}, nil
}

func (h *handlerManager) DeleteDevice(ctx context.Context, in *pb.DeviceIdentifier) (*empty.Empty, error) {
	dev, err := h.getDevice(ctx, in)
	if err != nil {
		return nil, core.BuildGRPCError(err)
	}
	_, err = h.deviceManager.DeleteDevice(ctx, &pb_lorawan.DeviceIdentifier{AppEui: &dev.AppEUI, DevEui: &dev.DevEUI})
	if err != nil {
		return nil, core.BuildGRPCError(errors.Wrap(core.FromGRPCError(err), "Broker did not delete device"))
	}
	err = h.handler.devices.Delete(in.AppId, in.DevId)
	if err != nil {
		return nil, core.BuildGRPCError(err)
	}
	return &empty.Empty{}, nil
}

func (h *handlerManager) GetDevicesForApplication(ctx context.Context, in *pb.ApplicationIdentifier) (*pb.DeviceList, error) {
	if !in.Validate() {
		return nil, grpcErrf(codes.InvalidArgument, "Invalid Application Identifier")
	}
	claims, err := h.handler.Component.ValidateTTNAuthContext(ctx)
	if err != nil {
		return nil, core.BuildGRPCError(err)
	}
	if !claims.CanEditApp(in.AppId) {
		return nil, grpcErrf(codes.PermissionDenied, "No access to this application")
	}
	devices, err := h.handler.devices.ListForApp(in.AppId)
	if err != nil {
		return nil, core.BuildGRPCError(err)
	}
	res := &pb.DeviceList{Devices: []*pb.Device{}}
	for _, dev := range devices {
		res.Devices = append(res.Devices, &pb.Device{
			AppId: dev.AppID,
			DevId: dev.DevID,
			Device: &pb.Device_LorawanDevice{LorawanDevice: &pb_lorawan.Device{
				AppId:   dev.AppID,
				AppEui:  &dev.AppEUI,
				DevId:   dev.DevID,
				DevEui:  &dev.DevEUI,
				DevAddr: &dev.DevAddr,
				NwkSKey: &dev.NwkSKey,
				AppSKey: &dev.AppSKey,
				AppKey:  &dev.AppKey,
			}},
		})
	}
	return res, nil
}

func (h *handlerManager) getApplication(ctx context.Context, in *pb.ApplicationIdentifier) (*application.Application, error) {
	if !in.Validate() {
		return nil, core.NewErrInvalidArgument("Application Identifier", "validation failed")
	}
	claims, err := h.handler.Component.ValidateTTNAuthContext(ctx)
	if err != nil {
		return nil, err
	}
	if !claims.CanEditApp(in.AppId) {
		return nil, core.NewErrPermissionDenied(fmt.Sprintf("No access to Application %s", in.AppId))
	}
	app, err := h.handler.applications.Get(in.AppId)
	if err != nil {
		return nil, err
	}
	if !claims.CanEditApp(app.AppID) {
		return nil, core.NewErrPermissionDenied(fmt.Sprintf("No access to Application %s", in.AppId))
	}
	return app, nil
}

func (h *handlerManager) GetApplication(ctx context.Context, in *pb.ApplicationIdentifier) (*pb.Application, error) {
	app, err := h.getApplication(ctx, in)
	if err != nil {
		return nil, core.BuildGRPCError(err)
	}

	return &pb.Application{
		AppId:     app.AppID,
		Decoder:   app.Decoder,
		Converter: app.Converter,
		Validator: app.Validator,
		Encoder:   app.Encoder,
	}, nil
}

func (h *handlerManager) RegisterApplication(ctx context.Context, in *pb.ApplicationIdentifier) (*empty.Empty, error) {
	app, err := h.getApplication(ctx, &pb.ApplicationIdentifier{AppId: in.AppId})
	if err != nil && core.GetErrType(err) != core.NotFound {
		return nil, core.BuildGRPCError(err)
	}
	if app != nil {
		return nil, grpcErrf(codes.AlreadyExists, "Application already registered")
	}

	err = h.handler.applications.Set(&application.Application{
		AppID: in.AppId,
	})
	if err != nil {
		return nil, core.BuildGRPCError(err)
	}

	md, _ := metadata.FromContext(ctx)
	token, _ := md["token"]
	err = h.handler.Discovery.AddMetadata(pb_discovery.Metadata_APP_ID, []byte(in.AppId), token[0])
	if err != nil {
		h.handler.Ctx.WithField("AppID", in.AppId).WithError(err).Warn("Could not register Application with Discovery")
	}

	_, err = h.handler.ttnBrokerManager.RegisterApplicationHandler(ctx, &pb_broker.ApplicationHandlerRegistration{
		AppId:     in.AppId,
		HandlerId: h.handler.Identity.Id,
	})
	if err != nil {
		h.handler.Ctx.WithField("AppID", in.AppId).WithError(err).Warn("Could not register Application with Broker")
	}

	return &empty.Empty{}, nil

}

func (h *handlerManager) SetApplication(ctx context.Context, in *pb.Application) (*empty.Empty, error) {
	_, err := h.getApplication(ctx, &pb.ApplicationIdentifier{AppId: in.AppId})
	if err != nil {
		return nil, core.BuildGRPCError(err)
	}

	if !in.Validate() {
		return nil, grpcErrf(codes.InvalidArgument, "Invalid Application")
	}

	err = h.handler.applications.Set(&application.Application{
		AppID:     in.AppId,
		Decoder:   in.Decoder,
		Converter: in.Converter,
		Validator: in.Validator,
		Encoder:   in.Encoder,
	})
	if err != nil {
		return nil, core.BuildGRPCError(err)
	}

	return &empty.Empty{}, nil
}

func (h *handlerManager) DeleteApplication(ctx context.Context, in *pb.ApplicationIdentifier) (*empty.Empty, error) {
	_, err := h.getApplication(ctx, in)
	if err != nil {
		return nil, core.BuildGRPCError(err)
	}

	// Get and delete all devices for this application
	devices, err := h.handler.devices.ListForApp(in.AppId)
	if err != nil {
		return nil, core.BuildGRPCError(err)
	}
	for _, dev := range devices {
		_, err = h.deviceManager.DeleteDevice(ctx, &pb_lorawan.DeviceIdentifier{AppEui: &dev.AppEUI, DevEui: &dev.DevEUI})
		if err != nil {
			return nil, core.BuildGRPCError(errors.Wrap(core.FromGRPCError(err), "Broker did not delete device"))
		}
		err = h.handler.devices.Delete(dev.AppID, dev.DevID)
		if err != nil {
			return nil, core.BuildGRPCError(err)
		}
	}

	// Delete the Application
	err = h.handler.applications.Delete(in.AppId)
	if err != nil {
		return nil, core.BuildGRPCError(err)
	}

	md, _ := metadata.FromContext(ctx)
	token, _ := md["token"]
	err = h.handler.Discovery.DeleteMetadata(pb_discovery.Metadata_APP_ID, []byte(in.AppId), token[0])
	if err != nil {
		h.handler.Ctx.WithField("AppID", in.AppId).WithError(core.FromGRPCError(err)).Warn("Could not unregister Application from Discovery")
	}

	return &empty.Empty{}, nil
}

func (h *handlerManager) GetPrefixes(ctx context.Context, in *pb_lorawan.PrefixesRequest) (*pb_lorawan.PrefixesResponse, error) {
	res, err := h.devAddrManager.GetPrefixes(ctx, in)
	if err != nil {
		return nil, core.BuildGRPCError(errors.Wrap(core.FromGRPCError(err), "Broker did not return prefixes"))
	}
	return res, nil
}

func (h *handlerManager) GetDevAddr(ctx context.Context, in *pb_lorawan.DevAddrRequest) (*pb_lorawan.DevAddrResponse, error) {
	res, err := h.devAddrManager.GetDevAddr(ctx, in)
	if err != nil {
		return nil, core.BuildGRPCError(errors.Wrap(core.FromGRPCError(err), "Broker did not return DevAddr"))
	}
	return res, nil
}

func (h *handlerManager) GetStatus(ctx context.Context, in *pb.StatusRequest) (*pb.Status, error) {
	return nil, grpcErrf(codes.Unimplemented, "Not Implemented")
}

func (h *handler) RegisterManager(s *grpc.Server) {
	server := &handlerManager{
		handler:        h,
		deviceManager:  pb_lorawan.NewDeviceManagerClient(h.ttnBrokerConn),
		devAddrManager: pb_lorawan.NewDevAddrManagerClient(h.ttnBrokerConn),
	}
	pb.RegisterHandlerManagerServer(s, server)
	pb.RegisterApplicationManagerServer(s, server)
	pb_lorawan.RegisterDevAddrManagerServer(s, server)
}