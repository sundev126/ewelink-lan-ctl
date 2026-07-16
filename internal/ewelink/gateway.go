package ewelink

import "context"

type TokenProvider interface {
	Access(context.Context) (string, string, error)
	ForceRefresh(context.Context) error
}

type DeviceClient interface {
	ListDevices(context.Context, string, string) ([]Device, error)
	GetDevice(context.Context, string, string, string) (Device, error)
	SetSwitch(context.Context, string, string, string, string) error
}

type Gateway struct {
	Tokens TokenProvider
	Client DeviceClient
}

func (g *Gateway) ListDevices(ctx context.Context) ([]Device, error) {
	return gatewayCall(ctx, g, func(region, token string) ([]Device, error) {
		return g.Client.ListDevices(ctx, region, token)
	})
}

func (g *Gateway) GetDevice(ctx context.Context, deviceID string) (Device, error) {
	return gatewayCall(ctx, g, func(region, token string) (Device, error) {
		return g.Client.GetDevice(ctx, region, token, deviceID)
	})
}

func (g *Gateway) SetSwitch(ctx context.Context, deviceID, state string) error {
	_, err := gatewayCall(ctx, g, func(region, token string) (struct{}, error) {
		return struct{}{}, g.Client.SetSwitch(ctx, region, token, deviceID, state)
	})
	return err
}

func gatewayCall[T any](ctx context.Context, gateway *Gateway, call func(string, string) (T, error)) (T, error) {
	region, token, err := gateway.Tokens.Access(ctx)
	if err != nil {
		var zero T
		return zero, err
	}
	result, err := call(region, token)
	if !IsTokenError(err) {
		return result, err
	}
	if err := gateway.Tokens.ForceRefresh(ctx); err != nil {
		var zero T
		return zero, err
	}
	region, token, err = gateway.Tokens.Access(ctx)
	if err != nil {
		var zero T
		return zero, err
	}
	return call(region, token)
}
