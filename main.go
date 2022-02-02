package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"io/ioutil"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"github.com/jhump/protoreflect/desc"
	"github.com/mwitkow/grpc-proxy/proxy"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

func getEnv(key string, defaultVal string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}

	return defaultVal
}

func main() {
	log.Println("hello, friend")

	confFile := getEnv("GRPC_GW_CONF", "./conf.yml")
	log.Println("reading config file ", confFile)

	appConfig, err := ReadConfig(confFile)
	if err != nil {
		panic(err)
	}

	// map for processed methods
	mProcessed := map[string]struct{}{}
	// mapping method => connection
	mapping := make(map[string]*grpc.ClientConn)
	// collected file descriptors
	fdsCollected := make([]*desc.FileDescriptor, 0)
	for _, cli := range appConfig.Endpoints {

		var opts []grpc.DialOption
		opts = append(opts, grpc.WithTimeout(42*time.Second))
		opts = append(opts, grpc.WithCodec(proxy.Codec()))
		opts = append(opts, grpc.WithBlock())

		if cli.CaCert == "" && cli.MyCert == "" && cli.MyCertKey == "" {
			log.Println("insecure endpoint")
			opts = append(opts, grpc.WithInsecure())
		} else if cli.CaCert != "" && cli.MyCert == "" && cli.MyCertKey == "" {
			log.Println("tls mode")
			creds, err := credentials.NewClientTLSFromFile(cli.CaCert, "")
			if err != nil {
				panic(err)
			}
			opts = append(opts, grpc.WithTransportCredentials(creds))
		} else if cli.CaCert != "" && cli.MyCert != "" && cli.MyCertKey != "" {
			log.Println("mtls mode")
			// mutual tls
			certificate, err := tls.LoadX509KeyPair(cli.MyCert, cli.MyCertKey)
			if err != nil {
				panic("Load client certification failed: " + err.Error())
			}

			ca, err := ioutil.ReadFile(cli.CaCert)
			if err != nil {
				panic(err)
			}

			caPool := x509.NewCertPool()
			if !caPool.AppendCertsFromPEM(ca) {
				panic("can't add CA cert")
			}

			tlsConfig := &tls.Config{
				ServerName:   "localhost", // TODO
				Certificates: []tls.Certificate{certificate},
				RootCAs:      caPool,
			}

			creds := credentials.NewTLS(tlsConfig)
			opts = append(opts, grpc.WithTransportCredentials(creds))
		} else {
			panic("wrong tls parameters")
		}

		log.Println("Dialing ", cli.Dial)
		conn, err := grpc.Dial(cli.Dial, opts...)
		if err != nil {
			panic(err)
		}

		err, methods, fds := DiscoverServices(conn)
		if err != nil {
			panic(err)
		}
		var processFds func(f *desc.FileDescriptor) []*desc.FileDescriptor
		processFds = func(f *desc.FileDescriptor) []*desc.FileDescriptor {
			var result []*desc.FileDescriptor
			result = append(result, f)
			for _, d := range f.GetDependencies() {
				result = append(result, processFds(d)...)
			}
			for _, d := range f.GetWeakDependencies() {
				result = append(result, processFds(d)...)
			}
			for _, d := range f.GetPublicDependencies() {
				result = append(result, processFds(d)...)
			}
			return result
		}
		for _, f := range fds {
			if strings.HasPrefix(f.GetPackage(), "grpc.reflection") {
				continue
			}

			fdsCollected = append(fdsCollected, processFds(f)...)
		}
		for _, m := range methods {
			log.Println("method discovered: ", m)
			if strings.HasPrefix(m, "/grpc.reflection.") {
				// ignore reflection.
				continue
			}
			if _, ok := mProcessed[m]; ok {
				panic("duplicate method discovered!: " + m)
			}
			mProcessed[m] = struct{}{}
			mapping[m] = conn
		}
	}

	director := func(ctx context.Context, fullMethodName string) (context.Context,
		*grpc.ClientConn, error) {

		log.Println("somebody calling: ", fullMethodName)

		// blacklist
		for _, bl := range appConfig.Blacklist {
			if fullMethodName == bl {
				log.Println("method is blacklisted")
				return ctx, nil, grpc.Errorf(codes.Unimplemented, "blacklisted")
			}
		}
		if conn, ok := mapping[fullMethodName]; ok {
			log.Println("method registered")
			md, ok := metadata.FromIncomingContext(ctx)
			if ok {
				outCtx, _ := context.WithCancel(ctx)
				outCtx = metadata.NewOutgoingContext(outCtx, md.Copy())
				return outCtx, conn, nil
			}
		}

		log.Println("unknown method")
		return ctx, nil, nil
	}

	srvConfig := appConfig.Server
	var opts []grpc.ServerOption
	opts = append(opts, grpc.CustomCodec(proxy.Codec()))
	opts = append(opts, grpc.UnknownServiceHandler(proxy.TransparentHandler(director)))

	lis, err := net.Listen("tcp", srvConfig.Listen)
	if err != nil {
		log.Fatalf("failed to listen: %v", err)
	}

	// server insecure/tls/mtls modes
	if srvConfig.CaCert == "" && srvConfig.MyCert == "" && srvConfig.MyCertKey == "" {
		log.Println("server is insecure")
		// opts already insecure
	} else if srvConfig.CaCert == "" && srvConfig.MyCert != "" && srvConfig.MyCertKey != "" {
		log.Println("server in tls mode")
		// server tls
		cert, err := tls.LoadX509KeyPair(srvConfig.MyCert, srvConfig.MyCertKey)
		if err != nil {
			panic(err)
		}
		opts = append(opts, grpc.Creds(credentials.NewServerTLSFromCert(&cert)))
	} else if srvConfig.CaCert != "" && srvConfig.MyCert != "" && srvConfig.MyCertKey != "" {
		log.Println("server use mutual tls")
		cert, err := tls.LoadX509KeyPair(srvConfig.MyCert, srvConfig.MyCertKey)
		if err != nil {
			panic(err)
		}
		certPool := x509.NewCertPool()
		ca, err := ioutil.ReadFile(srvConfig.CaCert)
		if err != nil {
			panic(err)
		}
		if ok := certPool.AppendCertsFromPEM(ca); !ok {
			panic("failed to append ca certificate")
		}
		opts = append(opts, grpc.Creds(credentials.NewTLS(&tls.Config{
			ClientAuth:   tls.RequireAndVerifyClientCert,
			Certificates: []tls.Certificate{cert},
			ClientCAs:    certPool,
		})))
	} else {
		panic("wrong server tls config")
	}

	s := grpc.NewServer(
		opts...,
	)

	log.Println("register custom reflection")

	log.Println("files collected: ")
	for _, f := range fdsCollected {
		log.Println("  ", f.GetFullyQualifiedName())
	}

	RegisterCustomReflection(s, fdsCollected)

	//RegisterOriginalReflection(s)

	log.Printf("server listening at %v", lis.Addr())
	if err := s.Serve(lis); err != nil {
		log.Fatalf("failed to serve: %v", err)
	}

	exit := make(chan bool)
	<-exit
}
