// Copyright (c) 2017-2018 The nox developers

package rpc

import (
	"sync"
)

// RpcServer provides a concurrent safe RPC server to a chain server.
type RpcServer struct {
	started                int32
	shutdown               int32
	numClients             int32
	statusLines            map[int]string
	statusLock             sync.RWMutex
	wg                     sync.WaitGroup
	requestProcessShutdown chan struct{}
	quit                   chan int
}


