package work

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"github.com/cenkalti/backoff/v4"
)

// DequeueFunc generates a job.
type DequeueFunc func(*DequeueOptions) (*Job, error)

// DequeueMiddleware modifies DequeueFunc behavior.
type DequeueMiddleware func(DequeueFunc) DequeueFunc

// EnqueueFunc takes in a job for processing.
type EnqueueFunc func(*Job, *EnqueueOptions) error

// EnqueueMiddleware modifies EnqueueFunc behavior.
type EnqueueMiddleware func(EnqueueFunc) EnqueueFunc

// HandleFunc runs a job.
type HandleFunc func(*Job, *DequeueOptions) error

// ContextHandleFunc runs a job.
type ContextHandleFunc func(context.Context, *Job, *DequeueOptions) error

// HandleMiddleware modifies HandleFunc hehavior.
type HandleMiddleware func(HandleFunc) HandleFunc

type handler struct {
	QueueID    string
	HandleFunc ContextHandleFunc
	JobOptions JobOptions
}

// WorkerOptions is used to create a worker.
type WorkerOptions struct {
	Namespace string
	Queue     Queue
	ErrorFunc func(error)
}

// Worker runs jobs.
type Worker struct {
	opt WorkerOptions

	cancel     func()
	wg         sync.WaitGroup
	handlerMap map[string]handler
}

// NewWorker creates a new worker.
func NewWorker(opt *WorkerOptions) *Worker {
	return &Worker{
		opt:        *opt,
		handlerMap: make(map[string]handler),
	}
}

// JobOptions specifies how a job is executed.
// It overrides default WorkerOptions so each handler can have different execution settings.
type JobOptions struct {
	WorkerOptions
	MaxExecutionTime time.Duration
	IdleWait         time.Duration
	NumGoroutines    int64

	DequeueMiddleware []DequeueMiddleware
	HandleMiddleware  []HandleMiddleware
}

// AddDequeueMiddleware adds DequeueMiddleware.
func (opt *JobOptions) AddDequeueMiddleware(mw DequeueMiddleware) *JobOptions {
	opt.DequeueMiddleware = append(opt.DequeueMiddleware, mw)
	return opt
}

// AddHandleMiddleware adds HandleMiddleware.
func (opt *JobOptions) AddHandleMiddleware(mw HandleMiddleware) *JobOptions {
	opt.HandleMiddleware = append(opt.HandleMiddleware, mw)
	return opt
}

// options validation error
var (
	ErrMaxExecutionTime = errors.New("work: max execution time should be > 0")
	ErrNumGoroutines    = errors.New("work: number of goroutines should be > 0")
	ErrIdleWait         = errors.New("work: idle wait should be > 0")
)

// Validate validates JobOptions.
func (opt *JobOptions) Validate() error {
	if opt.MaxExecutionTime <= 0 {
		return ErrMaxExecutionTime
	}
	if opt.IdleWait <= 0 {
		return ErrIdleWait
	}
	if opt.NumGoroutines <= 0 {
		return ErrNumGoroutines
	}
	return nil
}

var (
	// ErrDoNotRetry is returned if the job should not be retried;
	// this may be because the job is unrecoverable, or because
	// the handler has already rescheduled it.
	ErrDoNotRetry = errors.New("work: do not retry")

	// ErrQueueNotFound is returned if the queue is not yet
	// defined with Register().
	ErrQueueNotFound = errors.New("work: queue is not found")

	// ErrUnrecoverable is returned if the error is unrecoverable.
	// The job will be discarded.
	ErrUnrecoverable = fmt.Errorf("work: permanent error: %w", ErrDoNotRetry)

	// ErrUnsupported is returned if it is not implemented.
	ErrUnsupported = errors.New("work: unsupported")
)

// Register adds handler for a queue.
// queueID and namespace should be the same as the one used to enqueue.
func (w *Worker) Register(queueID string, h HandleFunc, opt *JobOptions) error {
	return w.RegisterWithContext(queueID, func(ctx context.Context, job *Job, o *DequeueOptions) error {
		return h(job, o)
	}, opt)
}

// RegisterWithContext adds handler for a queue with context.Context.
// queueID and namespace should be the same as the one used to enqueue.
// The context is created with context.WithTimeout set from MaxExecutionTime.
func (w *Worker) RegisterWithContext(queueID string, h ContextHandleFunc, opt *JobOptions) error {
	err := opt.Validate()
	if err != nil {
		return err
	}
	w.handlerMap[queueID] = handler{
		QueueID: queueID,
		HandleFunc: func(ctx context.Context, job *Job, o *DequeueOptions) error {
			ctx, cancel := context.WithTimeout(ctx, opt.MaxExecutionTime)
			defer cancel()
			return h(ctx, job, o)
		},
		JobOptions: *opt,
	}
	return nil
}

// Start starts the worker.
func (w *Worker) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	for _, h := range w.handlerMap {
		for i := int64(0); i < h.JobOptions.NumGoroutines; i++ {
			w.wg.Add(1)
			go w.start(ctx, h)
		}
	}
}

func (w *Worker) start(ctx context.Context, h handler) {
	defer w.wg.Done()

	queue := w.opt.Queue
	if h.JobOptions.Queue != nil {
		queue = h.JobOptions.Queue
	}
	ns := w.opt.Namespace
	if h.JobOptions.Namespace != "" {
		ns = h.JobOptions.Namespace
	}

	// print errors by default so that problems are noticeable.
	errFunc := func(err error) { fmt.Println(err) }
	if h.JobOptions.ErrorFunc != nil {
		errFunc = h.JobOptions.ErrorFunc
	} else if w.opt.ErrorFunc != nil {
		errFunc = w.opt.ErrorFunc
	}

	dequeue := getDequeueFunc(queue)
	for _, mw := range h.JobOptions.DequeueMiddleware {
		dequeue = mw(dequeue)
	}
	dequeue = idleWait(ctx, h.JobOptions.IdleWait)(dequeue)

	handle := func(job *Job, o *DequeueOptions) error {
		return h.HandleFunc(ctx, job, o)
	}
	for _, mw := range h.JobOptions.HandleMiddleware {
		handle = mw(handle)
	}
	handle = catchPanic(handle)
	handle = retry(queue)(handle)

	// prepare bulk ack flush
	var ackJobs []*Job
	flush := func() error {
		opt := &AckOptions{
			Namespace: ns,
			QueueID:   h.QueueID,
		}
		bulkDeq, ok := queue.(BulkDequeuer)
		if ok {
			err := bulkDeq.BulkAck(ackJobs, opt)
			if err != nil {
				return err
			}
			ackJobs = nil
			return nil
		}
		for _, job := range ackJobs {
			err := queue.Ack(job, opt)
			if err != nil {
				return err
			}
		}
		ackJobs = nil
		return nil
	}
	defer func() {
		err := flush()
		if err != nil {
			errFunc(err)
		}
	}()

	const flushIntv = time.Second
	flushTicker := time.NewTicker(flushIntv)
	defer flushTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-flushTicker.C:
			err := flush()
			if err != nil {
				errFunc(err)
			}
		default:
			func() error {
				opt := &DequeueOptions{
					Namespace:    ns,
					QueueID:      h.QueueID,
					At:           time.Now(),
					InvisibleSec: int64(2 * (h.JobOptions.MaxExecutionTime + flushIntv) / time.Second),
				}
				job, err := dequeue(opt)
				if err != nil {
					if !errors.Is(err, ErrEmptyQueue) {
						errFunc(err)
					}
					return err
				}
				err = handle(job, opt)
				if err != nil {
					return err
				}
				ackJobs = append(ackJobs, job)
				if len(ackJobs) >= 1000 {
					// prevent un-acked job count to be too large
					err := flush()
					if err != nil {
						errFunc(err)
						return err
					}
				}
				return nil
			}()
		}
	}
}

func getDequeueFunc(queue Queue) DequeueFunc {
	bulkDeq, ok := queue.(BulkDequeuer)
	if !ok {
		return queue.Dequeue
	}

	var jobs []*Job
	return func(opt *DequeueOptions) (*Job, error) {
		if len(jobs) == 0 {
			// this is an optimization to reduce system calls.
			//
			// there could be an idle period on startup
			// because worker previously pulls in too many jobs.
			count := 60 / opt.InvisibleSec
			if count <= 0 {
				count = 1
			}
			bulkOpt := *opt
			bulkOpt.InvisibleSec *= count

			var err error
			jobs, err = bulkDeq.BulkDequeue(count, &bulkOpt)
			if err != nil {
				return nil, err
			}
		}
		job := jobs[0]
		jobs = jobs[1:]
		return job, nil
	}
}

// ExportMetrics dumps queue stats if the queue implements MetricsExporter.
func (w *Worker) ExportMetrics() (*Metrics, error) {
	var queueMetrics []*QueueMetrics
	for _, h := range w.handlerMap {
		queue := w.opt.Queue
		if h.JobOptions.Queue != nil {
			queue = h.JobOptions.Queue
		}
		ns := w.opt.Namespace
		if h.JobOptions.Namespace != "" {
			ns = h.JobOptions.Namespace
		}
		exporter, ok := queue.(MetricsExporter)
		if !ok {
			continue
		}
		m, err := exporter.GetQueueMetrics(&QueueMetricsOptions{
			Namespace: ns,
			QueueID:   h.QueueID,
			At:        time.Now(),
		})
		if err != nil {
			return nil, err
		}
		queueMetrics = append(queueMetrics, m)
	}
	return &Metrics{
		Queue: queueMetrics,
	}, nil
}

// Stop stops the worker.
func (w *Worker) Stop() {
	if w.cancel != nil {
		w.cancel()
	}
	w.wg.Wait()
}

func idleWait(ctx context.Context, d time.Duration) DequeueMiddleware {
	return func(f DequeueFunc) DequeueFunc {
		return func(opt *DequeueOptions) (*Job, error) {
			job, err := f(opt)
			if err != nil {
				if errors.Is(err, ErrEmptyQueue) {
					select {
					case <-time.After(d):
					case <-ctx.Done():
					}
				}
				return nil, err
			}
			return job, nil
		}
	}
}

func catchPanic(f HandleFunc) HandleFunc {
	return func(job *Job, opt *DequeueOptions) (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic: %v\n\n%s", r, debug.Stack())
			}
		}()
		return f(job, opt)
	}
}

func retry(queue Queue) HandleMiddleware {
	return func(f HandleFunc) HandleFunc {
		return func(job *Job, opt *DequeueOptions) error {
			err := f(job, opt)
			if err != nil {
				if errors.Is(err, ErrUnrecoverable) {
					return nil // ack
				}
				if errors.Is(err, ErrDoNotRetry) {
					// don't ack and don't reenqueue
					return err
				}
				now := time.Now()
				job.Retries++
				job.LastError = err.Error()
				job.UpdatedAt = now

				// https://github.com/grpc/grpc/blob/master/doc/connection-backoff.md
				b := backoff.NewExponentialBackOff()
				b.InitialInterval = 2 * time.Second
				b.RandomizationFactor = 0.2
				b.Multiplier = 1.6
				b.MaxInterval = 24 * time.Hour
				b.MaxElapsedTime = 0
				b.Reset()

				var next time.Duration
				for i := int64(0); i < job.Retries; i++ {
					next = b.NextBackOff()
				}
				job.EnqueuedAt = now.Add(next)
				queue.Enqueue(job, &EnqueueOptions{
					Namespace: opt.Namespace,
					QueueID:   opt.QueueID,
				})
				return err
			}
			return nil
		}
	}
}
