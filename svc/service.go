// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// +build windows

// Package svc provides everything required to build Windows service.
//
package svc

import (
	"errors"
	"github.com/multiplay/winsvc/winapi"
	"runtime"
	"syscall"
	"unsafe"
)

// State describes service execution state (Stopped, Running and so on).
type State uint32

const (
	Stopped         = State(winapi.SERVICE_STOPPED)
	StartPending    = State(winapi.SERVICE_START_PENDING)
	StopPending     = State(winapi.SERVICE_STOP_PENDING)
	Running         = State(winapi.SERVICE_RUNNING)
	ContinuePending = State(winapi.SERVICE_CONTINUE_PENDING)
	PausePending    = State(winapi.SERVICE_PAUSE_PENDING)
	Paused          = State(winapi.SERVICE_PAUSED)
)

// Cmd represents service state change request. It is sent to a service
// by the service manager, and should be actioned upon by the service.
type Cmd uint32
type EventType uint32

const (
	Stop        = Cmd(winapi.SERVICE_CONTROL_STOP)
	Pause       = Cmd(winapi.SERVICE_CONTROL_PAUSE)
	Continue    = Cmd(winapi.SERVICE_CONTROL_CONTINUE)
	Interrogate = Cmd(winapi.SERVICE_CONTROL_INTERROGATE)
	Shutdown    = Cmd(winapi.SERVICE_CONTROL_SHUTDOWN)
	PreShutdown = Cmd(winapi.SERVICE_CONTROL_PRESHUTDOWN)
)

// Accepted is used to describe commands accepted by the service.
// Note, that Interrogate is always accepted.
type Accepted uint32

const (
	AcceptStop             = Accepted(winapi.SERVICE_ACCEPT_STOP)
	AcceptShutdown         = Accepted(winapi.SERVICE_ACCEPT_SHUTDOWN)
	AcceptPreShutdown      = Accepted(winapi.SERVICE_ACCEPT_PRESHUTDOWN)
	AcceptPauseAndContinue = Accepted(winapi.SERVICE_ACCEPT_PAUSE_CONTINUE)
)

// Status combines State and Accepted commands to fully describe running service.
type Status struct {
	State      State
	Accepts    Accepted
	CheckPoint uint32 // used to report progress during a lengthy operation
	WaitHint   uint32 // estimated time required for a pending operation, in milliseconds
}

// ChangeRequest is sent to service Handler to request service status change.
type ChangeRequest struct {
	Cmd           Cmd
	CurrentStatus Status
}

// Handler is the interface that must be implemented to build Windows service.
type Handler interface {

	// Execute will be called by the package code at the start of
	// the service, and the service will exit once Execute completes.
	// Inside Execute you must read service change requests from r and
	// act accordingly. You must keep service control manager up to date
	// about state of your service by writing into s as required.
	// args contains argument strings passed to the service.
	// You can provide service exit code in exitCode return parameter,
	// with 0 being "no error". You can also indicate if exit code,
	// if any, is service specific or not by using svcSpecificEC
	// parameter.
	Execute(args []string, r <-chan ChangeRequest, s chan<- Status) (svcSpecificEC bool, exitCode uint32)
}

var (
	// These are used by asm code.
	goWaitsH                       uintptr
	cWaitsH                        uintptr
	ssHandle                       uintptr
	sName                          *uint16
	sArgc                          uintptr
	sArgv                          **uint16
	ctlHandlerProc                 uintptr
	cSetEvent                      uintptr
	cWaitForSingleObject           uintptr
	cRegisterServiceCtrlHandlerExW uintptr
)

func init() {
	k := syscall.MustLoadDLL("kernel32.dll")
	cSetEvent = k.MustFindProc("SetEvent").Addr()
	cWaitForSingleObject = k.MustFindProc("WaitForSingleObject").Addr()
	a := syscall.MustLoadDLL("advapi32.dll")
	cRegisterServiceCtrlHandlerExW = a.MustFindProc("RegisterServiceCtrlHandlerExW").Addr()
}

type ctlEvent struct {
	cmd       Cmd
	eventType EventType
	eventData uintptr
	context   uintptr
	errno     uint32
}

// service provides access to windows service api.
type service struct {
	name    string
	h       syscall.Handle
	cWaits  *event
	goWaits *event
	c       chan ctlEvent
	handler Handler
}

func newService(name string, handler Handler) (*service, error) {
	var s service
	var err error
	s.name = name
	s.c = make(chan ctlEvent)
	s.handler = handler
	s.cWaits, err = newEvent()
	if err != nil {
		return nil, err
	}
	s.goWaits, err = newEvent()
	if err != nil {
		s.cWaits.Close()
		return nil, err
	}
	return &s, nil
}

func (s *service) close() error {
	s.cWaits.Close()
	s.goWaits.Close()
	return nil
}

type exitCode struct {
	isSvcSpecific bool
	errno         uint32
}

func (s *service) updateStatus(status *Status, ec *exitCode) error {
	if s.h == 0 {
		return errors.New("updateStatus with no service status handle")
	}
	var t winapi.SERVICE_STATUS
	t.ServiceType = winapi.SERVICE_WIN32_OWN_PROCESS
	t.CurrentState = uint32(status.State)
	if status.Accepts&AcceptStop != 0 {
		t.ControlsAccepted |= winapi.SERVICE_ACCEPT_STOP
	}
	if status.Accepts&AcceptShutdown != 0 {
		t.ControlsAccepted |= winapi.SERVICE_ACCEPT_SHUTDOWN
	}
	if status.Accepts&AcceptPreShutdown != 0 {
		t.ControlsAccepted |= winapi.SERVICE_ACCEPT_PRESHUTDOWN
	}
	if status.Accepts&AcceptPauseAndContinue != 0 {
		t.ControlsAccepted |= winapi.SERVICE_ACCEPT_PAUSE_CONTINUE
	}
	if ec.errno == 0 {
		t.Win32ExitCode = winapi.NO_ERROR
		t.ServiceSpecificExitCode = winapi.NO_ERROR
	} else if ec.isSvcSpecific {
		t.Win32ExitCode = uint32(winapi.ERROR_SERVICE_SPECIFIC_ERROR)
		t.ServiceSpecificExitCode = ec.errno
	} else {
		t.Win32ExitCode = ec.errno
		t.ServiceSpecificExitCode = winapi.NO_ERROR
	}
	t.CheckPoint = status.CheckPoint
	t.WaitHint = status.WaitHint
	return winapi.SetServiceStatus(s.h, &t)
}

const (
	sysErrSetServiceStatusFailed = uint32(syscall.APPLICATION_ERROR) + iota
	sysErrNewThreadInCallback
)

func (s *service) run() {
	s.goWaits.Wait()
	s.h = syscall.Handle(ssHandle)
	argv := (*[100]*int16)(unsafe.Pointer(sArgv))[:sArgc]
	args := make([]string, len(argv))
	for i, a := range argv {
		args[i] = syscall.UTF16ToString((*[1 << 20]uint16)(unsafe.Pointer(a))[:])
	}

	cmdsToHandler := make(chan ChangeRequest)
	changesFromHandler := make(chan Status)
	exitFromHandler := make(chan exitCode)

	go func() {
		ss, errno := s.handler.Execute(args, cmdsToHandler, changesFromHandler)
		exitFromHandler <- exitCode{ss, errno}
	}()

	status := Status{State: Stopped}
	ec := exitCode{isSvcSpecific: true, errno: 0}
	var outch chan ChangeRequest
	inch := s.c
	var cmd Cmd
loop:
	for {
		select {
		case r := <-inch:
			if r.errno != 0 {
				ec.errno = r.errno
				break loop
			}
			inch = nil
			outch = cmdsToHandler
			cmd = r.cmd
		case outch <- ChangeRequest{cmd, status}:
			inch = s.c
			outch = nil
		case c := <-changesFromHandler:
			err := s.updateStatus(&c, &ec)
			if err != nil {
				// best suitable error number
				ec.errno = sysErrSetServiceStatusFailed
				if err2, ok := err.(syscall.Errno); ok {
					ec.errno = uint32(err2)
				}
				break loop
			}
			status = c
		case ec = <-exitFromHandler:
			break loop
		}
	}

	s.updateStatus(&Status{State: Stopped}, &ec)
	s.cWaits.Set()
}

func newCallback(fn interface{}) (cb uintptr, err error) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}
		cb = 0
		switch v := r.(type) {
		case string:
			err = errors.New(v)
		case error:
			err = v
		default:
			err = errors.New("unexpected panic in syscall.NewCallback")
		}
	}()
	return syscall.NewCallback(fn), nil
}

// BUG(brainman): There is no mechanism to run multiple services
// inside one single executable. Perhaps, it can be overcome by
// using RegisterServiceCtrlHandlerEx Windows api.

// Run executes service named name by calling appropriate handler function.
func Run(name string, handler Handler) error {
	runtime.LockOSThread()

	tid := winapi.GetCurrentThreadId()

	s, err := newService(name, handler)
	if err != nil {
		return err
	}

	ctlHandler := func(ctl uint32, event uint32, eventData uintptr, ctx uintptr) uintptr {
		e := ctlEvent{cmd: Cmd(ctl), eventType: EventType(event), eventData: eventData, context: ctx}
		// We assume that this callback function is running on
		// the same thread as Run. Nowhere in MS documentation
		// I could find statement to guarantee that. So putting
		// check here to verify, otherwise things will go bad
		// quickly, if ignored.
		i := winapi.GetCurrentThreadId()
		if i != tid {
			e.errno = sysErrNewThreadInCallback
		}
		s.c <- e
		return 0
	}

	var svcmain uintptr
	getServiceMain(&svcmain)
	t := []winapi.SERVICE_TABLE_ENTRY{
		{syscall.StringToUTF16Ptr(s.name), svcmain},
		{nil, 0},
	}

	goWaitsH = uintptr(s.goWaits.h)
	cWaitsH = uintptr(s.cWaits.h)
	sName = t[0].ServiceName
	ctlHandlerProc, err = newCallback(ctlHandler)
	if err != nil {
		return err
	}

	go s.run()

	err = winapi.StartServiceCtrlDispatcher(&t[0])
	if err != nil {
		return err
	}
	return nil
}
