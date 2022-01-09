package main

import (
	"context"
	"errors"
	"strings"

	"github.com/jhump/protoreflect/desc"
	"github.com/jhump/protoreflect/grpcreflect"
	gr "github.com/jhump/protoreflect/grpcreflect"
	"google.golang.org/grpc"
	rpb "google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
	"google.golang.org/grpc/status"
)

// this file is used to retrieve information,
// such as services list, proto- files
// from grpc client connections

// code was taken from evans sources
// https://github.com/ktr0731/evans/blob/master/grpc/grpcreflection/reflection.go

var ErrTLSHandshakeFailed = errors.New("TLS handshake failed")

func ListPackages(c *gr.Client) ([]*desc.FileDescriptor, error) {
	ssvcs, err := c.ListServices()
	if err != nil {
		msg := status.Convert(err).Message()
		// Check whether the error message contains TLS related error.
		// If the server didn't enable TLS, the error message contains the first string.
		// If Evans didn't enable TLS against to the TLS enabled server, the error message contains
		// the second string.
		if strings.Contains(msg, "tls: first record does not look like a TLS handshake") ||
			strings.Contains(msg, "latest connection error: <nil>") {
			return nil, ErrTLSHandshakeFailed
		}
		return nil, err
	}

	fds := make([]*desc.FileDescriptor, 0, len(ssvcs))
	for _, s := range ssvcs {
		svc, err := c.ResolveService(s)
		if err != nil {
			if gr.IsElementNotFoundError(err) {
				// Service doesn't expose the ServiceDescriptor, skip.
				continue
			}
			return nil, err
		}
		f := svc.GetFile()
		fds = append(fds, f)
	}

	return fds, nil
}

func DiscoverServices(conn *grpc.ClientConn) (error, []string, []*desc.FileDescriptor) {

	stub := rpb.NewServerReflectionClient(conn)

	rclient := grpcreflect.NewClient(context.Background(), stub)

	fds, err := ListPackages(rclient)
	if err != nil {
		return err, []string{}, []*desc.FileDescriptor{}
	}

	services, err := rclient.ListServices()
	if err != nil {
		return err, []string{}, []*desc.FileDescriptor{}
	}

	var methods []string
	for _, srv := range services {
		sdes, err := rclient.ResolveService(srv)
		if err == nil {
			for _, m := range sdes.GetMethods() {
				fullMethodName := "/" + srv + "/" + m.GetName()
				methods = append(methods, fullMethodName)
			}
		}
	}
	return nil, methods, fds
}
