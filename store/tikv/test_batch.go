package tikv

import (
	"context"
	"io"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pingcap/errors"
	"github.com/pingcap/kvproto/pkg/tikvpb"
	"github.com/pingcap/tidb/config"
	"github.com/pingcap/tidb/store/tikv/tikvrpc"
	"github.com/pingcap/tidb/util/logutil"
	"go.uber.org/zap"
	grpcCodes "google.golang.org/grpc/codes"
	grpcStatus "google.golang.org/grpc/status"
)

// batchClientTester is a thin wrap over the rpcClient used to test command batching
type batchClientTester struct {
	rpcClient Client
	cfg       BatchClientTestConfig
	failed    uint32
	ended     uint32
	wait      sync.WaitGroup
}

// BatchClientTestConfig is used to config batchClientTester
type BatchClientTestConfig struct {
	Concurrent uint64
	Timeout    time.Duration
	MinDelay   uint64
	MaxDelay   uint64
	TestLength time.Duration
}

func (cfg *BatchClientTestConfig) genDelay() uint64 {
	r := rand.Int63n(int64(cfg.MaxDelay - cfg.MinDelay))
	return uint64(r) + cfg.MinDelay
}

// BatchTest start sending test messages to TiKV servers
func BatchTest(security config.Security, addrs []string, config BatchClientTestConfig) bool {
	c := &batchClientTester{
		rpcClient: newRPCClient(security),
		cfg:       config,
		failed:    0,
		ended:     0,
	}
	c.wait.Add(len(addrs) * int(c.cfg.Concurrent))
	for _, addr := range addrs {
		for i := uint64(0); i < c.cfg.Concurrent; i++ {
			go c.runTest(addr, i, c.cfg.Concurrent)
		}
	}
	<-time.After(c.cfg.TestLength)
	c.end()
	c.wait.Wait()
	done := c.unblockedClose()
	select {
	case <-done:
		// closed normally
	case <-time.After(100 * time.Millisecond):
		// do not close in time!
		logger().Error("The rpcClient do not close in time! Maybe there is goroutine leaking")
		c.fail()
	}
	return c.isFailed()
}

type sendRequestResult struct {
	*tikvrpc.Response
	error
}

// logger get default logger
func logger() *zap.SugaredLogger {
	return logutil.Logger(context.Background()).Sugar()
}

// fail make the test as failed
func (c *batchClientTester) fail() {
	atomic.StoreUint32(&c.failed, 1)
}

// isFailed get is it failed
func (c *batchClientTester) isFailed() bool {
	return atomic.LoadUint32(&c.failed) != 0
}

// end make the test end
func (c *batchClientTester) end() {
	atomic.StoreUint32(&c.ended, 1)
}

// isEnded get is it ended
func (c *batchClientTester) isEnded() bool {
	return atomic.LoadUint32(&c.ended) != 0
}

// unblockedClose close the client, but returns a channel instead of blocking the current goroutine
func (c *batchClientTester) unblockedClose() <-chan struct{} {
	done := make(chan struct{}, 1)
	go func() {
		c.rpcClient.Close()
		done <- struct{}{}
	}()
	return done
}

// test sends test messages to the server and process and check respond
func (c *batchClientTester) test(addr string, id uint64) {
	logger().Debugf("Invoke test RPC %d at %v", id, addr)
	req := &tikvrpc.Request{
		Type: tikvrpc.CmdBatchTest,
		BatchTest: &tikvpb.BatchCommandTestRequest{
			TestId:    id,
			DelayTime: c.cfg.genDelay(),
		},
	}
	result, err := c.rpcClient.SendRequest(context.Background(), addr, req, c.cfg.Timeout)
	if err != nil {
		err = errors.Cause(err)
		if status, ok := grpcStatus.FromError(err); ok {
			// grpc error
			switch status.Code() {
			case grpcCodes.DeadlineExceeded:
				// just time out
				logger().Warnf("Test RPC %d at %v timeout. error: %v", id, addr, err)
				return
			case grpcCodes.Unavailable:
				if strings.Contains(status.Message(), "all SubConns are in TransientFailure") {
					// server not availiable
					logger().Warnf("Test RPC %d at %v can not complete since connection to the server is not available. error: %v", id, addr, err)
					return
				} else if strings.Contains(status.Message(), "transport is closing") {
					// server is just killed, or panic (and the grpc thread killed)
					logger().Warnf("Test RPC %d at %v can not complete since the connection is unexpected closed by server. error: %v", id, addr, err)
					return
				}
			case grpcCodes.Internal:
				if strings.Contains(status.Message(), "RST_STREAM") {
					// seems like the server gracefully reset the connection when manually exited?
					logger().Warnf("Test RPC %d at %v is dropped since the stream is resetted. error: %v", id, addr, err)
					return
				}
			}
		}
		if err == io.EOF {
			// I have no idea why this happens in normal run, but it is returned when the connection is previously destroyed
			logger().Warnf("Test RPC %d at %v is can not complete because of a previously error. error: %v", id, addr, err)
		}
		logger().Errorf("Test RPC %d at %v have an unknown error: %v", id, addr, err)
		c.fail()
		return
	}
	res := result.Test
	if res == nil {
		// wrong response type
		logger().Errorf("Test RPC %d at %v returned wrong type of response", id, addr)
		c.fail()
		return
	}
	if res.TestId != id {
		// wrong id
		logger().Errorf("Test RPC %d at %v returned wrong id %d", id, addr, res.TestId)
		c.fail()
		return
	}
}

// runTest run test for one TiKV server
func (c *batchClientTester) runTest(addr string, idStart uint64, idInterval uint64) {
	defer func() { c.wait.Done() }()
	id := idStart
	for {
		if c.isEnded() {
			return
		}
		c.test(addr, id)
		id += idInterval
	}
}
