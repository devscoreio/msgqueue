package memqueue_test

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-redis/redis"
	"github.com/go-redis/redis_rate"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"golang.org/x/time/rate"

	"github.com/devscoreio/msgqueue"
	"github.com/devscoreio/msgqueue/memqueue"
)

func TestMemqueue(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "memqueue")
}

var _ = BeforeSuite(func() {
	msgqueue.SetLogger(nil)
})

var _ = Describe("message with args", func() {
	ch := make(chan bool, 10)
	handler := func(s string, i int) {
		Expect(s).To(Equal("string"))
		Expect(i).To(Equal(42))
		ch <- true
	}

	BeforeEach(func() {
		q := memqueue.NewQueue(&msgqueue.Options{
			Handler: handler,
		})
		q.Call("string", 42)

		err := q.Close()
		Expect(err).NotTo(HaveOccurred())
	})

	It("handler is called with args", func() {
		Expect(ch).To(Receive())
		Expect(ch).NotTo(Receive())
	})
})

var _ = Describe("message with invalid number of args", func() {
	ch := make(chan bool, 10)
	handler := func(s string) {
		ch <- true
	}

	BeforeEach(func() {
		q := memqueue.NewQueue(&msgqueue.Options{
			Handler:    handler,
			RetryLimit: 1,
		})
		q.Processor().Stop()

		err := q.Call()
		Expect(err).NotTo(HaveOccurred())

		err = q.Processor().ProcessOne()
		Expect(err).To(MatchError("msgqueue: arg=0 decoding failed: EOF"))

		err = q.Processor().ProcessAll()
		Expect(err).NotTo(HaveOccurred())

		err = q.Close()
		Expect(err).NotTo(HaveOccurred())
	})

	It("handler is not called", func() {
		Expect(ch).NotTo(Receive())
	})
})

var _ = Describe("HandlerFunc", func() {
	ch := make(chan bool, 10)
	handler := func(msg *msgqueue.Message) error {
		Expect(msg.Args).To(Equal([]interface{}{"string", 42}))
		Expect(msg.Body).To(BeEmpty())
		ch <- true
		return nil
	}

	BeforeEach(func() {
		q := memqueue.NewQueue(&msgqueue.Options{
			Handler: msgqueue.HandlerFunc(handler),
		})
		q.Call("string", 42)

		err := q.Close()
		Expect(err).NotTo(HaveOccurred())
	})

	It("is called with Message", func() {
		Expect(ch).To(Receive())
		Expect(ch).NotTo(Receive())
	})
})

var _ = Describe("message retry timing", func() {
	var q *memqueue.Queue
	backoff := 100 * time.Millisecond

	var count int
	var ch chan time.Time
	handler := func() error {
		ch <- time.Now()
		count++
		return fmt.Errorf("fake error #%d", count)
	}

	BeforeEach(func() {
		count = 0
		ch = make(chan time.Time, 10)
		q = memqueue.NewQueue(&msgqueue.Options{
			Handler:    handler,
			RetryLimit: 3,
			MinBackoff: backoff,
		})
	})

	Context("without delay", func() {
		var now time.Time

		BeforeEach(func() {
			now = time.Now()
			q.Call()

			err := q.Close()
			Expect(err).NotTo(HaveOccurred())
		})

		It("is retried in time", func() {
			Expect(ch).To(Receive(BeTemporally("~", now, backoff/10)))
			Expect(ch).To(Receive(BeTemporally("~", now.Add(backoff), backoff/10)))
			Expect(ch).To(Receive(BeTemporally("~", now.Add(3*backoff), backoff/10)))
			Expect(ch).NotTo(Receive())
		})
	})

	Context("message with delay", func() {
		var now time.Time

		BeforeEach(func() {
			msg := msgqueue.NewMessage()
			msg.Delay = 5 * backoff
			now = time.Now().Add(msg.Delay)

			q.Add(msg)

			err := q.Close()
			Expect(err).NotTo(HaveOccurred())
		})

		It("is retried in time", func() {
			Expect(ch).To(Receive(BeTemporally("~", now, backoff/10)))
			Expect(ch).To(Receive(BeTemporally("~", now.Add(backoff), backoff/10)))
			Expect(ch).To(Receive(BeTemporally("~", now.Add(3*backoff), backoff/10)))
			Expect(ch).NotTo(Receive())
		})
	})

	Context("with NoDelay=true", func() {
		BeforeEach(func() {
			err := q.Close()
			Expect(err).NotTo(HaveOccurred())

			q = memqueue.NewQueue(&msgqueue.Options{
				Handler:    handler,
				RetryLimit: 3,
				MinBackoff: backoff,
			})
			q.SetNoDelay(true)
		})

		It("is processed immediately", func() {
			now := time.Now()

			msg := msgqueue.NewMessage()
			msg.Delay = time.Hour
			err := q.Add(msg)
			Expect(err).NotTo(HaveOccurred())

			err = q.Close()
			Expect(err).NotTo(HaveOccurred())

			Expect(ch).To(Receive(BeTemporally("~", now, backoff/10)))
			Expect(ch).To(Receive(BeTemporally("~", now, backoff/10)))
			Expect(ch).To(Receive(BeTemporally("~", now, backoff/10)))
			Expect(ch).NotTo(Receive())
		})
	})
})

var _ = Describe("failing queue with error handler", func() {
	var q *memqueue.Queue

	handler := func() error {
		return errors.New("fake error")
	}

	ch := make(chan bool, 10)
	fallbackHandler := func() {
		ch <- true
	}

	BeforeEach(func() {
		q = memqueue.NewQueue(&msgqueue.Options{
			Handler:         handler,
			FallbackHandler: fallbackHandler,
			RetryLimit:      1,
		})
		q.Call()

		err := q.Close()
		Expect(err).NotTo(HaveOccurred())
	})

	It("error handler is called when handler fails", func() {
		Expect(ch).To(Receive())
		Expect(ch).NotTo(Receive())
	})
})

var _ = Describe("named message", func() {
	var count int64
	handler := func() {
		atomic.AddInt64(&count, 1)
	}

	BeforeEach(func() {
		q := memqueue.NewQueue(&msgqueue.Options{
			Redis:   redisRing(),
			Handler: handler,
		})

		var wg sync.WaitGroup
		for i := 0; i < 100; i++ {
			wg.Add(1)
			go func() {
				defer GinkgoRecover()
				defer wg.Done()
				msg := msgqueue.NewMessage()
				msg.Name = "myname"
				q.Add(msg)
			}()
		}
		wg.Wait()

		err := q.Close()
		Expect(err).NotTo(HaveOccurred())
	})

	It("is processed once", func() {
		n := atomic.LoadInt64(&count)
		Expect(n).To(Equal(int64(1)))
	})
})

var _ = Describe("CallOnce", func() {
	var now time.Time
	delay := 3 * time.Second

	ch := make(chan time.Time, 10)
	handler := func(slot int64) error {
		ch <- time.Now()
		return nil
	}

	BeforeEach(func() {
		now = time.Now()

		q := memqueue.NewQueue(&msgqueue.Options{
			Redis:   redisRing(),
			Handler: handler,
		})

		var wg sync.WaitGroup
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer GinkgoRecover()
				defer wg.Done()

				q.CallOnce(delay, slot(delay))
			}()
		}
		wg.Wait()

		err := q.Close()
		Expect(err).NotTo(HaveOccurred())
	})

	It("processes message once with delay", func() {
		Expect(ch).To(Receive(BeTemporally(">", now.Add(delay), time.Second)))
		Consistently(ch).ShouldNot(Receive())
	})
})

var _ = Describe("stress testing", func() {
	var q *memqueue.Queue
	const n = 10000

	var count int64
	handler := func() {
		atomic.AddInt64(&count, 1)
	}

	BeforeEach(func() {
		q = memqueue.NewQueue(&msgqueue.Options{
			Handler: handler,
		})

		for i := 0; i < n; i++ {
			q.Call()
		}

		err := q.Close()
		Expect(err).NotTo(HaveOccurred())
	})

	It("handler is called for all messages", func() {
		nn := atomic.LoadInt64(&count)
		Expect(nn).To(Equal(int64(n)))
	})
})

var _ = Describe("stress testing failing queue", func() {
	var q *memqueue.Queue
	const n = 100000

	handler := func() error {
		return errors.New("fake error")
	}

	var errorCount int64
	fallbackHandler := func() {
		atomic.AddInt64(&errorCount, 1)
	}

	BeforeEach(func() {
		q = memqueue.NewQueue(&msgqueue.Options{
			Handler:              handler,
			FallbackHandler:      fallbackHandler,
			RetryLimit:           1,
			PauseErrorsThreshold: -1,
		})

		for i := 0; i < n; i++ {
			q.Call()
		}

		err := q.Close()
		Expect(err).NotTo(HaveOccurred())
	})

	It("error handler is called for all messages", func() {
		nn := atomic.LoadInt64(&errorCount)
		Expect(nn).To(Equal(int64(n)))
	})
})

var _ = Describe("empty queue", func() {
	var q *memqueue.Queue
	var processed uint32

	BeforeEach(func() {
		processed = 0
		q = memqueue.NewQueue(&msgqueue.Options{
			Redis: redisRing(),
			Handler: func() {
				atomic.AddUint32(&processed, 1)
			},
		})
	})

	AfterEach(func() {
		Expect(q.Close()).NotTo(HaveOccurred())
	})

	It("can be closed", func() {
		err := q.Close()
		Expect(err).NotTo(HaveOccurred())
	})

	It("stops processor", func() {
		err := q.Processor().Stop()
		Expect(err).NotTo(HaveOccurred())
	})

	testEmptyQueue := func() {
		It("processes all messages", func() {
			err := q.Processor().ProcessAll()
			Expect(err).NotTo(HaveOccurred())
		})

		It("processes one message", func() {
			err := q.Processor().ProcessOne()
			Expect(err).To(MatchError("msgqueue: queue is empty"))

			err = q.Processor().ProcessAll()
			Expect(err).NotTo(HaveOccurred())
		})
	}

	testEmptyQueue()

	Context("when processor is stopped", func() {
		BeforeEach(func() {
			err := q.Processor().Stop()
			Expect(err).NotTo(HaveOccurred())
		})

		testEmptyQueue()

		Context("when there are messages in the queue", func() {
			BeforeEach(func() {
				for i := 0; i < 3; i++ {
					err := q.Call()
					Expect(err).NotTo(HaveOccurred())
				}
			})

			It("processes all messages", func() {
				p := q.Processor()

				err := p.ProcessAll()
				Expect(err).NotTo(HaveOccurred())

				n := atomic.LoadUint32(&processed)
				Expect(n).To(Equal(uint32(3)))
			})

			It("processes one message", func() {
				p := q.Processor()

				err := p.ProcessOne()
				Expect(err).NotTo(HaveOccurred())

				n := atomic.LoadUint32(&processed)
				Expect(n).To(Equal(uint32(1)))

				err = p.ProcessAll()
				Expect(err).NotTo(HaveOccurred())

				n = atomic.LoadUint32(&processed)
				Expect(n).To(Equal(uint32(3)))
			})
		})
	})
})

// slot splits time into equal periods (called slots) and returns
// slot number for provided time.
func slot(period time.Duration) int64 {
	tm := time.Now()
	periodSec := int64(period / time.Second)
	if periodSec == 0 {
		return tm.Unix()
	}
	return tm.Unix() / periodSec
}

type memqueueStorage struct {
	*redis.Ring
}

func (c memqueueStorage) Exists(key string) bool {
	return !c.SetNX(key, "", 12*time.Hour).Val()
}

func redisRing() *redis.Ring {
	ring := redis.NewRing(&redis.RingOptions{
		Addrs:    map[string]string{"0": ":6379"},
		PoolSize: 100,
	})
	err := ring.FlushDb().Err()
	if err != nil {
		panic(err)
	}
	return ring
}

func rateLimiter() *redis_rate.Limiter {
	l := redis_rate.NewLimiter(redisRing())
	l.Fallback = rate.NewLimiter(rate.Every(time.Millisecond), 100)
	return l
}
