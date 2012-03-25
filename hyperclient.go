package hyperclient

/*
#cgo LDFLAGS: -lhyperclient
#include <netinet/in.h>
#include "hyperclient.h"

struct hyperclient_attribute* GetAttribute(struct hyperclient_attribute* list, int i) {
	return &list[i];
}

*/
import "C"

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"unsafe"
)

// CHANNEL_BUFFER_SIZE is the size of all the returned channels' buffer.
// You can set it to 0 if you want unbuffered channels.
var CHANNEL_BUFFER_SIZE = 1

// Timeout in miliseconds.
// Negative timeout means no timeout.
const hyperclient_loop_timeout = -1

const (
	datatype_STRING  = 8960
	datatype_INT64   = 8961
	datatype_GARBAGE = 9087
)

const (
	returncode_SUCCESS      = 8448
	returncode_NOTFOUND     = 8449
	returncode_SEARCHDONE   = 8450
	returncode_CMPFAIL      = 8451
	returncode_UNKNOWNSPACE = 8512
	returncode_COORDFAIL    = 8513
	returncode_SERVERERROR  = 8514
	returncode_CONNECTFAIL  = 8515
	returncode_DISCONNECT   = 8516
	returncode_RECONFIGURE  = 8517
	returncode_LOGICERROR   = 8518
	returncode_TIMEOUT      = 8519
	returncode_UNKNOWNATTR  = 8520
	returncode_DUPEATTR     = 8521
	returncode_SEEERRNO     = 8522
	returncode_NONEPENDING  = 8523
	returncode_DONTUSEKEY   = 8524
	returncode_WRONGTYPE    = 8525
	returncode_EXCEPTION    = 8574
	returncode_ZERO         = 8575
	returncode_A            = 8576
	returncode_B            = 8577
)

var internalErrorMessages map[int64]string = map[int64]string{
	returncode_SUCCESS:      "Success",
	returncode_NOTFOUND:     "Not Found",
	returncode_SEARCHDONE:   "Search Done",
	returncode_CMPFAIL:      "Conditional Operation Did Not Match Object",
	returncode_UNKNOWNSPACE: "Unknown Space",
	returncode_COORDFAIL:    "Coordinator Failure",
	returncode_SERVERERROR:  "Server Error",
	returncode_CONNECTFAIL:  "Connection Failure",
	returncode_DISCONNECT:   "Connection Reset",
	returncode_RECONFIGURE:  "Reconfiguration",
	returncode_LOGICERROR:   "Logic Error (file a bug)",
	returncode_TIMEOUT:      "Timeout",
	returncode_UNKNOWNATTR:  "Unknown attribute '%s'",
	returncode_DUPEATTR:     "Duplicate attribute '%s'",
	returncode_SEEERRNO:     "See ERRNO",
	returncode_NONEPENDING:  "None pending",
	returncode_DONTUSEKEY:   "Do not specify the key in a search predicate and do not redundantly specify the key for an insert",
	returncode_WRONGTYPE:    "Attribute '%s' has the wrong type",
	returncode_EXCEPTION:    "Internal Error (file a bug)",
}

// Client is the hyperdex client used to make requests to hyperdex.
type Client struct {
	ptr       *C.struct_hyperclient
	requests  []request
	closeChan chan struct{}
}

// Attributes represents a map of key-value attribute pairs.
//
// The value must be either a string or an int64-compatible integer
// (int, int8, int16, int32, int64, uint8, uint16, uint32).
// An incompatible type will NOT result in a panic but in a regular error return.
//
// Please note that there is no support for uint64 since its negative might be incorrectly evaluated.
// Support for uint has been dropped because it is unspecified whether it is 32 or 64 bits.
type Attributes map[string]interface{}

type Object struct {
	Key   string
	Attrs Attributes
}

type ObjectChannel <-chan *Object
type ErrorChannel <-chan error

type bundle map[string]interface{}

type request struct {
	id      int64
	objch   chan *Object
	errch   chan error
	bundle  bundle
	success func(request)
	failure func(request, C.enum_hyperclient_returncode)
}

// NewClient initializes a hyperdex client ready to use.
//
// For every call to NewClient, there must be a call to Destroy.
//
// Panics when the internal looping goroutine receives an error from hyperdex.
//
// Example:
// 		client, err := hyperclient.NewClient("127.0.0.1", 1234)
// 		if err != nil {
//			//handle error
//		}
//		defer client.Destroy()
//		// use client
func NewClient(ip string, port int) (*Client, error) {
	C_client := C.hyperclient_create(C.CString(ip), C.in_port_t(port))
	if C_client == nil {
		return nil, fmt.Errorf("Could not create hyperclient (ip=%s, port=%d)", ip, port)
	}
	client := &Client{
		C_client,
		make([]request, 0, 8), // No reallocation within 8 concurrent requests to hyperclient_loop
		make(chan struct{}, 1),
	}

	go func() {
		for {
			select {
			// quit goroutine when client is destroyed
			case <-client.closeChan:
				return
			default:
				// check if there are pending requests
				// and only if there are, call hyperclient_loop
				if l := len(client.requests); l > 0 {
					var status C.enum_hyperclient_returncode
					ret := int64(C.hyperclient_loop(client.ptr, hyperclient_loop_timeout, &status))
					if ret < 0 && status != returncode_TIMEOUT {
						panic(newInternalError(status).Error())
					}
					// find processed request among pending requests
					for i, req := range client.requests {
						if req.id == ret {
							if status == returncode_SUCCESS {
								req.success(req)
							} else {
								req.failure(req, status)
							}
							// remove processed request from pending requests
							client.requests = append(client.requests[:i], client.requests[i+1:]...)
							break
						}
					}
				}
				// prevent other goroutines from starving
				runtime.Gosched()
			}
		}
		panic("Should not be reached: end of infinite loop")
	}()

	return client, nil
}

// Destroy closes the connection between the Client and hyperdex. It has to be used on a client that is not used anymore.
//
// For every call to NewClient, there must be a call to Destroy.
func (client *Client) Destroy() {
	close(client.closeChan)
	C.hyperclient_destroy(client.ptr)
}

func (client *Client) AtomicInc(space, key string, attrs Attributes) ErrorChannel {
	return client.atomicIncDec(space, key, attrs, false)
}

func (client *Client) AtomicDec(space, key string, attrs Attributes) ErrorChannel {
	return client.atomicIncDec(space, key, attrs, true)
}

func (client *Client) Get(space, key string) (ObjectChannel, ErrorChannel) {
	errch := make(chan error, CHANNEL_BUFFER_SIZE)
	objch := make(chan *Object, CHANNEL_BUFFER_SIZE)
	var status C.enum_hyperclient_returncode
	var C_attrs *C.struct_hyperclient_attribute
	var C_attrs_sz C.size_t
	req_id := int64(C.hyperclient_get(client.ptr, C.CString(space), C.CString(key), C.size_t(len([]byte(key))), &status, &C_attrs, &C_attrs_sz))
	if req_id < 0 {
		errch <- newInternalError(status)
		close(errch)
		close(objch)
		return objch, errch
	}
	req := request{
		req_id,
		objch,
		errch,
		bundle{"key": key, "status": &status, "C_attrs": &C_attrs, "C_attrs_sz": &C_attrs_sz},
		func(req request) {
			C_attrs := *req.bundle["C_attrs"].(**C.struct_hyperclient_attribute)
			C_attrs_sz := *req.bundle["C_attrs_sz"].(*C.size_t)
			attrs, err := newAttributeListFromC(C_attrs, C_attrs_sz)
			if err != nil {
				req.errch <- err
				close(req.errch)
				close(req.objch)
				return
			}
			C.hyperclient_destroy_attrs(C_attrs, C_attrs_sz)
			req.objch <- &Object{req.bundle["key"].(string), attrs}
			req.errch <- nil
			close(req.objch)
			close(req.errch)
		},
		failureTwoChannels,
	}
	client.requests = append(client.requests, req)
	return objch, errch
}

func (client *Client) Delete(space, key string) ErrorChannel {
	errch := make(chan error, CHANNEL_BUFFER_SIZE)
	var status C.enum_hyperclient_returncode
	req_id := int64(C.hyperclient_del(client.ptr, C.CString(space), C.CString(key), C.size_t(len([]byte(key))), &status))
	if req_id < 0 {
		errch <- newInternalError(status)
		close(errch)
		return errch
	}
	req := request{
		req_id,
		nil,
		errch,
		nil,
		nil,
		failureOneChannel,
	}
	client.requests = append(client.requests, req)
	return errch
}

func newInternalError(status C.enum_hyperclient_returncode, a ...interface{}) error {
	s, ok := internalErrorMessages[int64(status)]
	if ok {
		return fmt.Errorf(s, a)
	}
	return errors.New("Unknown Error (file a bug)")
}

func (client *Client) atomicIncDec(space, key string, attrs Attributes, negative bool) ErrorChannel {
	errch := make(chan error, CHANNEL_BUFFER_SIZE)
	var status C.enum_hyperclient_returncode
	C_attr, err := getOneCTypeAttribute(attrs, negative)
	if err != nil {
		errch <- err
		close(errch)
		return errch
	}
	req_id := int64(C.hyperclient_atomicinc(client.ptr, C.CString(space), C.CString(key), C.size_t(len(key)), C_attr, C.size_t(1), &status))
	if req_id < 0 {
		errch <- newInternalError(status, C.GoString(C_attr.attr))
		close(errch)
		return errch
	}
	req := request{
		req_id,
		nil,
		errch,
		nil,
		nil,
		failureOneChannel,
	}
	client.requests = append(client.requests, req)
	return errch
}

func failureOneChannel(req request, status C.enum_hyperclient_returncode) {
	req.errch <- newInternalError(status)
	close(req.errch)
}

func failureTwoChannels(req request, status C.enum_hyperclient_returncode) {
	req.errch <- newInternalError(status)
	close(req.errch)
	close(req.objch)
}

func newCTypeAttributeList(attrs Attributes) (ret *C.struct_hyperclient_attribute, size uint, err error) {
	C_attrs := make([]C.struct_hyperclient_attribute, len(attrs))
	for key, value := range attrs {
		attr, err := newCTypeAttribute(key, value, false)
		if err != nil {
			return nil, 0, err
		}
		C_attrs = append(C_attrs, *attr)
		size += 1
	}
	if size == 0 {
		return nil, size, nil
	}
	return &C_attrs[0], size, nil
}

func newCTypeAttribute(key string, value interface{}, negative bool) (*C.struct_hyperclient_attribute, error) {
	var val string
	var datatype C.enum_hyperclient_returncode
	size := 0

	switch v := value.(type) {
	case string:
		val = v
		datatype = datatype_STRING
		size = len([]byte(val))
	case int, int8, int16, int32, int64, uint8, uint16, uint32:
		var i int64
		// Converting all int64-compatible integers to int64
		switch v := v.(type) {
		case int:
			i = int64(v)
		case int8:
			i = int64(v)
		case int16:
			i = int64(v)
		case int32:
			i = int64(v)
		case int64:
			i = v
		case uint8:
			i = int64(v)
		case uint16:
			i = int64(v)
		case uint32:
			i = int64(v)
		default:
			panic("Should not be reached: normalizing integers to int64")
		}

		if negative {
			i = -i
		}
		// Binary encoding
		buf := new(bytes.Buffer)
		err := binary.Write(buf, binary.LittleEndian, i)
		if err != nil {
			return nil, fmt.Errorf("Could not convert integer '%d' to bytes", i)
		}
		val = buf.String()
		datatype = datatype_INT64
		size = binary.Size(int64(0))
	default:
		return nil, fmt.Errorf("Attribute with key '%s' has unsupported type '%T'", key, v)
	}
	return &C.struct_hyperclient_attribute{
		C.CString(key),
		C.CString(val),
		C.size_t(size),
		C.enum_hyperclient_returncode(datatype),
		[4]byte{}, // alignment
	}, nil
}

func getOneCTypeAttribute(attrs Attributes, negative bool) (*C.struct_hyperclient_attribute, error) {
	for key, value := range attrs {
		return newCTypeAttribute(key, value, negative)
	}
	return nil, fmt.Errorf("Could not retrieve one CTypeAttribute from %v", attrs)
}

func newAttributeListFromC(C_attrs *C.struct_hyperclient_attribute, C_attrs_sz C.size_t) (attrs Attributes, err error) {
	attrs = Attributes{}
	for i := 0; i < int(C_attrs_sz); i++ {
		C_attr := C.GetAttribute(C_attrs, C.int(i))
		attr := C.GoString(C_attr.attr)
		switch C_attr.datatype {
		case datatype_STRING:
			attrs[attr] = C.GoStringN(C_attr.value, C.int(C_attr.value_sz))
		case datatype_INT64:
			var value int64
			buf := bytes.NewBuffer(C.GoBytes(unsafe.Pointer(C_attr.value), C.int(C_attr.value_sz)))
			err := binary.Read(buf, binary.LittleEndian, &value)
			if err != nil {
				return nil, fmt.Errorf("Could not decode INT64 attribute `%s` (#%d)", attr, i)
			}
			attrs[attr] = value
		case datatype_GARBAGE:
			continue
		default:
			return nil, fmt.Errorf("Unknown datatype %d found for attribute `%s` (#%d)", C_attr.datatype, attr, i)
		}
	}
	return attrs, nil
}
