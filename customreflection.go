/*

	Modified.
	original file:
	https://github.com/grpc/grpc-go/blob/v1.43.0/reflection/serverreflection.go

*/

package main

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"reflect"
	"sort"
	"sync"

	"github.com/golang/protobuf/proto"
	dpb "github.com/golang/protobuf/protoc-gen-go/descriptor"
	"github.com/jhump/protoreflect/desc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	rpb "google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
	"google.golang.org/grpc/status"
)

// GRPCServer is the interface provided by a gRPC server. It is implemented by
// *grpc.Server, but could also be implemented by other concrete types. It acts
// as a registry, for accumulating the services exposed by the server.
type GRPCServer interface {
	grpc.ServiceRegistrar
	GetServiceInfo() map[string]grpc.ServiceInfo
}

var _ GRPCServer = (*grpc.Server)(nil)

type serverReflectionServer struct {
	rpb.UnimplementedServerReflectionServer
	s GRPCServer

	fds          map[string]*desc.FileDescriptor
	initSymbols  sync.Once
	symbols      map[string]*dpb.FileDescriptorProto // map of fully-qualified names to files
	serviceNames []string
}

// Register registers the server reflection service on the given gRPC server.
func RegisterCustomReflection(s GRPCServer, fds []*desc.FileDescriptor) {
	srs := &serverReflectionServer{
		s:   s,
		fds: make(map[string]*desc.FileDescriptor),
	}
	for _, f := range fds {
		if _, ok := srs.fds[f.GetName()]; !ok {
			srs.fds[f.GetName()] = f
		}
	}
	rpb.RegisterServerReflectionServer(s, srs)
}

// protoMessage is used for type assertion on proto messages.
// Generated proto message implements function Descriptor(), but Descriptor()
// is not part of interface proto.Message. This interface is needed to
// call Descriptor().
type protoMessage interface {
	Descriptor() ([]byte, []int)
}

func (s *serverReflectionServer) getSymbols() (svcNames []string, symbolIndex map[string]*dpb.FileDescriptorProto) {
	s.initSymbols.Do(func() {
		s.symbols = map[string]*dpb.FileDescriptorProto{}
		s.serviceNames = []string{}
		fProcessed := map[string]struct{}{}
		sProcessed := map[string]struct{}{}
		for _, fd := range s.fds {
			// for each file descriptor
			//           get services, push it's name to serviceNames
			// done: processFile(fd)
			ssvcs := fd.GetServices()
			for _, svc := range ssvcs {
				fqn := svc.GetFullyQualifiedName()

				// if there is duplicated service fqn, don't add it to slice
				// e.g. reflection used in multiple services
				if _, ok := sProcessed[fqn]; !ok {
					s.serviceNames = append(s.serviceNames, fqn)
					sProcessed[fqn] = struct{}{}
				}
			}
			s.processFile(fd.AsFileDescriptorProto(), fProcessed)
		}

		sort.Strings(s.serviceNames)
	})

	keys := make([]string, 0, len(s.symbols))
	for k := range s.symbols {
		keys = append(keys, k)
	}

	return s.serviceNames, s.symbols
}

func (s *serverReflectionServer) processFile(fd *dpb.FileDescriptorProto, processed map[string]struct{}) {
	filename := fd.GetName()
	log.Println("processing file: ", filename)
	if _, ok := processed[filename]; ok {
		return
	}
	processed[filename] = struct{}{}

	prefix := fd.GetPackage()

	for _, msg := range fd.MessageType {
		s.processMessage(fd, prefix, msg)
	}
	for _, en := range fd.EnumType {
		s.processEnum(fd, prefix, en)
	}
	for _, ext := range fd.Extension {
		s.processField(fd, prefix, ext)
	}
	for _, svc := range fd.Service {
		svcName := fqn(prefix, svc.GetName())
		s.symbols[svcName] = fd
		log.Println("methods for service ", svcName)
		for _, meth := range svc.Method {
			name := fqn(svcName, meth.GetName())
			log.Println("  ", name)
			s.symbols[name] = fd
		}
	}

	for _, dep := range fd.Dependency {
		fdenc := proto.FileDescriptor(dep)
		fdDep, err := decodeFileDesc(fdenc)
		if err != nil {
			continue
		}
		s.processFile(fdDep, processed)
	}
}

func (s *serverReflectionServer) processMessage(fd *dpb.FileDescriptorProto, prefix string, msg *dpb.DescriptorProto) {
	msgName := fqn(prefix, msg.GetName())
	s.symbols[msgName] = fd

	for _, nested := range msg.NestedType {
		s.processMessage(fd, msgName, nested)
	}
	for _, en := range msg.EnumType {
		s.processEnum(fd, msgName, en)
	}
	for _, ext := range msg.Extension {
		s.processField(fd, msgName, ext)
	}
	for _, fld := range msg.Field {
		s.processField(fd, msgName, fld)
	}
	for _, oneof := range msg.OneofDecl {
		oneofName := fqn(msgName, oneof.GetName())
		s.symbols[oneofName] = fd
	}
}

func (s *serverReflectionServer) processEnum(fd *dpb.FileDescriptorProto, prefix string, en *dpb.EnumDescriptorProto) {
	enName := fqn(prefix, en.GetName())
	s.symbols[enName] = fd

	for _, val := range en.Value {
		valName := fqn(enName, val.GetName())
		s.symbols[valName] = fd
	}
}

func (s *serverReflectionServer) processField(fd *dpb.FileDescriptorProto, prefix string, fld *dpb.FieldDescriptorProto) {
	fldName := fqn(prefix, fld.GetName())
	s.symbols[fldName] = fd
}

func fqn(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

// fileDescForType gets the file descriptor for the given type.
// The given type should be a proto message.
func (s *serverReflectionServer) fileDescForType(st reflect.Type) (*dpb.FileDescriptorProto, error) {
	m, ok := reflect.Zero(reflect.PtrTo(st)).Interface().(protoMessage)
	if !ok {
		return nil, fmt.Errorf("failed to create message from type: %v", st)
	}
	enc, _ := m.Descriptor()

	return decodeFileDesc(enc)
}

// decodeFileDesc does decompression and unmarshalling on the given
// file descriptor byte slice.
func decodeFileDesc(enc []byte) (*dpb.FileDescriptorProto, error) {
	raw, err := decompress(enc)
	if err != nil {
		return nil, fmt.Errorf("failed to decompress enc: %v", err)
	}

	fd := new(dpb.FileDescriptorProto)
	if err := proto.Unmarshal(raw, fd); err != nil {
		return nil, fmt.Errorf("bad descriptor: %v", err)
	}
	return fd, nil
}

// decompress does gzip decompression.
func decompress(b []byte) ([]byte, error) {
	r, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("bad gzipped descriptor: %v", err)
	}
	out, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("bad gzipped descriptor: %v", err)
	}
	return out, nil
}

func typeForName(name string) (reflect.Type, error) {
	pt := proto.MessageType(name)
	if pt == nil {
		return nil, fmt.Errorf("unknown type: %q", name)
	}
	st := pt.Elem()

	return st, nil
}

func fileDescContainingExtension(st reflect.Type, ext int32) (*dpb.FileDescriptorProto, error) {
	m, ok := reflect.Zero(reflect.PtrTo(st)).Interface().(proto.Message)
	if !ok {
		return nil, fmt.Errorf("failed to create message from type: %v", st)
	}

	var extDesc *proto.ExtensionDesc
	for id, desc := range proto.RegisteredExtensions(m) {
		if id == ext {
			extDesc = desc
			break
		}
	}

	if extDesc == nil {
		return nil, fmt.Errorf("failed to find registered extension for extension number %v", ext)
	}

	return decodeFileDesc(proto.FileDescriptor(extDesc.Filename))
}

func (s *serverReflectionServer) allExtensionNumbersForType(st reflect.Type) ([]int32, error) {
	m, ok := reflect.Zero(reflect.PtrTo(st)).Interface().(proto.Message)
	if !ok {
		return nil, fmt.Errorf("failed to create message from type: %v", st)
	}

	exts := proto.RegisteredExtensions(m)
	out := make([]int32, 0, len(exts))
	for id := range exts {
		out = append(out, id)
	}
	return out, nil
}

// fileDescWithDependencies returns a slice of serialized fileDescriptors in
// wire format ([]byte). The fileDescriptors will include fd and all the
// transitive dependencies of fd with names not in sentFileDescriptors.
func fileDescWithDependencies(fd *dpb.FileDescriptorProto, sentFileDescriptors map[string]bool) ([][]byte, error) {
	r := [][]byte{}
	queue := []*dpb.FileDescriptorProto{fd}
	for len(queue) > 0 {
		currentfd := queue[0]
		queue = queue[1:]
		if sent := sentFileDescriptors[currentfd.GetName()]; len(r) == 0 || !sent {
			sentFileDescriptors[currentfd.GetName()] = true
			currentfdEncoded, err := proto.Marshal(currentfd)
			if err != nil {
				return nil, err
			}
			r = append(r, currentfdEncoded)
		}
		for _, dep := range currentfd.Dependency {
			fdenc := proto.FileDescriptor(dep)
			fdDep, err := decodeFileDesc(fdenc)
			if err != nil {
				continue
			}
			queue = append(queue, fdDep)
		}
	}
	return r, nil
}

// fileDescEncodingByFilename finds the file descriptor for given filename,
// finds all of its previously unsent transitive dependencies, does marshalling
// on them, and returns the marshalled result.
func (s *serverReflectionServer) fileDescEncodingByFilename(name string, sentFileDescriptors map[string]bool) ([][]byte, error) {
	// use s.fds instead
	fd, ok := s.fds[name]
	if ok {
		return fileDescWithDependencies(fd.AsFileDescriptorProto(), sentFileDescriptors)
	}
	return nil, fmt.Errorf("unknown file: %v", name)
}

// parseMetadata finds the file descriptor bytes specified meta.
// For SupportPackageIsVersion4, m is the name of the proto file, we
// call proto.FileDescriptor to get the byte slice.
// For SupportPackageIsVersion3, m is a byte slice itself.
func parseMetadata(meta interface{}) ([]byte, bool) {
	// Check if meta is the file name.
	if fileNameForMeta, ok := meta.(string); ok {
		return proto.FileDescriptor(fileNameForMeta), true
	}

	// Check if meta is the byte slice.
	if enc, ok := meta.([]byte); ok {
		return enc, true
	}

	return nil, false
}

// fileDescEncodingContainingSymbol finds the file descriptor containing the
// given symbol, finds all of its previously unsent transitive dependencies,
// does marshalling on them, and returns the marshalled result. The given symbol
// can be a type, a service or a method.
func (s *serverReflectionServer) fileDescEncodingContainingSymbol(name string, sentFileDescriptors map[string]bool) ([][]byte, error) {
	_, symbols := s.getSymbols()
	fd := symbols[name]
	if fd == nil {
		// Check if it's a type name that was not present in the
		// transitive dependencies of the registered services.
		if st, err := typeForName(name); err == nil {
			fd, err = s.fileDescForType(st)
			if err != nil {
				return nil, err
			}
		}
	}

	if fd == nil {
		return nil, fmt.Errorf("unknown symbol: %v", name)
	}

	return fileDescWithDependencies(fd, sentFileDescriptors)
}

// fileDescEncodingContainingExtension finds the file descriptor containing
// given extension, finds all of its previously unsent transitive dependencies,
// does marshalling on them, and returns the marshalled result.
func (s *serverReflectionServer) fileDescEncodingContainingExtension(typeName string, extNum int32, sentFileDescriptors map[string]bool) ([][]byte, error) {
	st, err := typeForName(typeName)
	if err != nil {
		return nil, err
	}
	fd, err := fileDescContainingExtension(st, extNum)
	if err != nil {
		return nil, err
	}
	return fileDescWithDependencies(fd, sentFileDescriptors)
}

// allExtensionNumbersForTypeName returns all extension numbers for the given type.
func (s *serverReflectionServer) allExtensionNumbersForTypeName(name string) ([]int32, error) {
	st, err := typeForName(name)
	if err != nil {
		return nil, err
	}
	extNums, err := s.allExtensionNumbersForType(st)
	if err != nil {
		return nil, err
	}
	return extNums, nil
}

// ServerReflectionInfo is the reflection service handler.
func (s *serverReflectionServer) ServerReflectionInfo(stream rpb.ServerReflection_ServerReflectionInfoServer) error {
	sentFileDescriptors := make(map[string]bool)
	for {
		in, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		out := &rpb.ServerReflectionResponse{
			ValidHost:       in.Host,
			OriginalRequest: in,
		}
		switch req := in.MessageRequest.(type) {
		case *rpb.ServerReflectionRequest_FileByFilename:
			b, err := s.fileDescEncodingByFilename(req.FileByFilename, sentFileDescriptors)
			if err != nil {
				out.MessageResponse = &rpb.ServerReflectionResponse_ErrorResponse{
					ErrorResponse: &rpb.ErrorResponse{
						ErrorCode:    int32(codes.NotFound),
						ErrorMessage: err.Error(),
					},
				}
			} else {
				out.MessageResponse = &rpb.ServerReflectionResponse_FileDescriptorResponse{
					FileDescriptorResponse: &rpb.FileDescriptorResponse{FileDescriptorProto: b},
				}
			}
		case *rpb.ServerReflectionRequest_FileContainingSymbol:
			b, err := s.fileDescEncodingContainingSymbol(req.FileContainingSymbol, sentFileDescriptors)
			if err != nil {
				out.MessageResponse = &rpb.ServerReflectionResponse_ErrorResponse{
					ErrorResponse: &rpb.ErrorResponse{
						ErrorCode:    int32(codes.NotFound),
						ErrorMessage: err.Error(),
					},
				}
			} else {
				out.MessageResponse = &rpb.ServerReflectionResponse_FileDescriptorResponse{
					FileDescriptorResponse: &rpb.FileDescriptorResponse{FileDescriptorProto: b},
				}
			}
		case *rpb.ServerReflectionRequest_FileContainingExtension:
			typeName := req.FileContainingExtension.ContainingType
			extNum := req.FileContainingExtension.ExtensionNumber
			b, err := s.fileDescEncodingContainingExtension(typeName, extNum, sentFileDescriptors)
			if err != nil {
				out.MessageResponse = &rpb.ServerReflectionResponse_ErrorResponse{
					ErrorResponse: &rpb.ErrorResponse{
						ErrorCode:    int32(codes.NotFound),
						ErrorMessage: err.Error(),
					},
				}
			} else {
				out.MessageResponse = &rpb.ServerReflectionResponse_FileDescriptorResponse{
					FileDescriptorResponse: &rpb.FileDescriptorResponse{FileDescriptorProto: b},
				}
			}
		case *rpb.ServerReflectionRequest_AllExtensionNumbersOfType:
			extNums, err := s.allExtensionNumbersForTypeName(req.AllExtensionNumbersOfType)
			if err != nil {
				out.MessageResponse = &rpb.ServerReflectionResponse_ErrorResponse{
					ErrorResponse: &rpb.ErrorResponse{
						ErrorCode:    int32(codes.NotFound),
						ErrorMessage: err.Error(),
					},
				}
			} else {
				out.MessageResponse = &rpb.ServerReflectionResponse_AllExtensionNumbersResponse{
					AllExtensionNumbersResponse: &rpb.ExtensionNumberResponse{
						BaseTypeName:    req.AllExtensionNumbersOfType,
						ExtensionNumber: extNums,
					},
				}
			}
		case *rpb.ServerReflectionRequest_ListServices:
			svcNames, _ := s.getSymbols()
			serviceResponses := make([]*rpb.ServiceResponse, len(svcNames))
			for i, n := range svcNames {
				serviceResponses[i] = &rpb.ServiceResponse{
					Name: n,
				}
			}
			out.MessageResponse = &rpb.ServerReflectionResponse_ListServicesResponse{
				ListServicesResponse: &rpb.ListServiceResponse{
					Service: serviceResponses,
				},
			}
		default:
			return status.Errorf(codes.InvalidArgument, "invalid MessageRequest: %v", in.MessageRequest)
		}

		if err := stream.Send(out); err != nil {
			return err
		}
	}
}
