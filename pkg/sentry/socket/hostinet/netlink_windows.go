// Copyright 2026 The gVisor Authors.
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

//go:build windows

package hostinet

import (
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/sentry/inet"
	"gvisor.dev/gvisor/pkg/sentry/socket/netlink/nlmsg"
)

// gvisor-windows: hostinet is not used on Windows (no host netlink); every
// call fails with ENOSYS so the package compiles.

func getInterfaces() (map[int32]inet.Interface, error) { return nil, unix.ENOSYS }

func getInterfaceAddrs() (map[int32][]inet.InterfaceAddr, error) { return nil, unix.ENOSYS }

func getRoutes() ([]inet.Route, error) { return nil, unix.ENOSYS }

func removeInterface(idx int32) error { return unix.ENOSYS }

func addInterfaceAddr(idx int32, addr inet.InterfaceAddr) error { return unix.ENOSYS }

func removeInterfaceAddr(idx int32, addr inet.InterfaceAddr) error { return unix.ENOSYS }

func doRouteRequest(typ uint16, msg *nlmsg.Message) error { return unix.ENOSYS }
