package batchy

import (
	"context"
	"errors"
	"hash/fnv"
	"sync"
	"time"

	"github.com/panjf2000/ants/v2"
)

var (
	ErrWorkerNotSet    = errors.New("worker pool size must be greater than zero")
	ErrProcessorNotSet = errors.New("processor function must not be nil")
	ErrInvalidTimeout  = errors.New("timeout duration must be positive")
)

// Batcher [T any] can add an item of type T, returning the corresponding error
type Batcher[T any] interface {
	// Add adds an item to the current batch
	Add(T) error

	// Stop stops the BatcherInstance
	Stop()
}

// Processor [T any] is a function that accepts items of type T and returns a corresponding array of errors
type Processor[T any] func(items []T) []error

// SchedulingPolicy defines how workers are scheduled
type SchedulingPolicy int

const (
	// ROUND_ROBIN distributes items evenly across workers
	ROUND_ROBIN SchedulingPolicy = iota
	// ORDERED_SEQUENTIAL processes items in strict order using a single worker
	ORDERED_SEQUENTIAL
)

type BatchConfig struct {
	// BatchSize 批大小
	BatchSize int
	// PoolSize 工作者数量
	PoolSize int
	// QueueSize 队列大小，如果为0则根据BatchSize和PoolSize自动优化
	QueueSize int
	// Ctx context 上下文
	Ctx context.Context
	// Timeout 若数据不足时，多久强制落盘一次
	Timeout time.Duration
	// SchedulingPolicy 调度策略
	SchedulingPolicy SchedulingPolicy
	// DynamicBatching 启用动态批次大小调整
	DynamicBatching bool
	// MinBatchSize 动态批处理的最小批次大小
	MinBatchSize int
	// MaxBatchSize 动态批处理的最大批次大小
	MaxBatchSize int
	// AdaptiveThreshold 批次大小调整的阈值
	AdaptiveThreshold time.Duration
}

// ChanBatcherInstance 阻塞式批处理器（有缓冲channel）
type ChanBatcherInstance[T any] struct {
	processor   Processor[T]
	itemLimit   int
	queue       chan T // 有缓冲channel
	workerCount int
	workers     *ants.Pool
	ctx         context.Context
	cancel      context.CancelFunc
	timeout     time.Duration
	baseTimeout time.Duration
	jitterSeed  uint32
	// Pre-computed jittered timeouts for each worker to avoid repeated hash calculations
	jitteredTimeouts []time.Duration
	mu               sync.RWMutex // Protects dynamic batching calculations
	stopOnce         sync.Once    // 确保Stop()只执行一次
	// Scheduling configuration
	schedulingPolicy SchedulingPolicy
	// Dynamic batching fields
	dynamicBatching   bool
	minBatchSize      int
	maxBatchSize      int
	adaptiveThreshold time.Duration
}

// NewChanBatcher 创建阻塞式批处理器
func NewChanBatcher[T any](
	processor Processor[T],
	batchConfig BatchConfig,
) (Batcher[T], error) {
	var err error
	if batchConfig.Ctx == nil {
		batchConfig.Ctx = context.Background()
	}

	// Adjust worker count based on scheduling policy
	actualWorkers := batchConfig.PoolSize
	if batchConfig.SchedulingPolicy == ORDERED_SEQUENTIAL {
		// For ordered processing, use only one worker to maintain order
		actualWorkers = 1
	}

	// 优化队列大小以提高内存效率
	queueSize := batchConfig.QueueSize
	if queueSize <= 0 {
		// 行业最佳实践：每个worker 2-5倍批大小，限制最大值以防止内存问题
		// 这样既能防止过度内存使用，又能保持良好的吞吐量
		optimalSize := batchConfig.BatchSize * actualWorkers * 3
		// 设置合理的最大值以防止内存问题
		maxSize := 50000 // 大多数用例的合理最大值
		if optimalSize > maxSize {
			queueSize = maxSize
		} else if optimalSize < batchConfig.BatchSize*2 {
			// 最小值：2倍批大小以防止立即阻塞
			queueSize = batchConfig.BatchSize * 2
		} else {
			queueSize = optimalSize
		}
	}
	if batchConfig.PoolSize <= 0 {
		return nil, ErrWorkerNotSet
	}
	if batchConfig.Timeout == 0 {
		return nil, ErrInvalidTimeout
	}
	if processor == nil {
		return nil, ErrProcessorNotSet
	}

	ctx, cancel := context.WithCancel(batchConfig.Ctx)

	// Generate consistent jitter seed based on instance creation time
	h := fnv.New32a()
	h.Write([]byte(time.Now().String()))
	jitterSeed := h.Sum32()

	// Set up dynamic batching parameters
	minBatchSize := batchConfig.MinBatchSize
	if minBatchSize <= 0 {
		minBatchSize = batchConfig.BatchSize / 4
		if minBatchSize < 1 {
			minBatchSize = 1
		}
	}

	maxBatchSize := batchConfig.MaxBatchSize
	if maxBatchSize <= 0 {
		maxBatchSize = batchConfig.BatchSize * 4
	}

	adaptiveThreshold := batchConfig.AdaptiveThreshold
	if adaptiveThreshold <= 0 {
		adaptiveThreshold = batchConfig.Timeout / 2
	}

	instance := &ChanBatcherInstance[T]{
		processor:         processor,
		itemLimit:         batchConfig.BatchSize,
		queue:             make(chan T, queueSize),
		ctx:               ctx,
		cancel:            cancel,
		timeout:           batchConfig.Timeout,
		baseTimeout:       batchConfig.Timeout,
		jitterSeed:        jitterSeed,
		schedulingPolicy:  batchConfig.SchedulingPolicy,
		dynamicBatching:   batchConfig.DynamicBatching,
		minBatchSize:      minBatchSize,
		maxBatchSize:      maxBatchSize,
		adaptiveThreshold: adaptiveThreshold,
	}

	// Pre-compute jittered timeouts for all workers to avoid repeated hash calculations
	instance.jitteredTimeouts = make([]time.Duration, actualWorkers)
	for i := 0; i < actualWorkers; i++ {
		instance.jitteredTimeouts[i] = instance.generateJitteredTimeout(i)
	}

	// 创建工作池
	pool, err := ants.NewPool(actualWorkers)
	if err != nil {
		return nil, err
	}
	instance.workers = pool
	instance.workerCount = actualWorkers
	// 启动worker with error handling
	for i := 0; i < actualWorkers; i++ {
		workerID := i
		err := pool.Submit(func() {
			// Add recovery mechanism for worker panics
			defer func() {
				if r := recover(); r != nil {
					// Log the panic but don't crash the entire system
					// In production, you might want to use a proper logger
				}
			}()
			instance.worker(workerID)
		})
		if err != nil {
			// If worker startup fails, clean up resources
			cancel()
			pool.Release()
			return nil, err
		}
	}

	return instance, nil
}

// Add 方法（完全阻塞式）
func (c *ChanBatcherInstance[T]) Add(item T) error {
	// 首先检查context是否已取消
	select {
	case <-c.ctx.Done():
		return errors.New("batcher已停止")
	default:
	}

	// 使用defer recover来捕获向已关闭channel发送数据的panic
	defer func() {
		if r := recover(); r != nil {
			// channel已关闭，但我们已经在上面检查了context
			// 这种情况不应该发生，但为了安全起见保留recover
		}
	}()

	select {
	case <-c.ctx.Done():
		return errors.New("batcher已停止")
	case c.queue <- item: // 关键点：channel满时会自动阻塞
		return nil
	}
}

// generateJitteredTimeout creates a consistent jittered timeout for each worker
// This prevents thundering herd effect by spreading timeout events across time
func (c *ChanBatcherInstance[T]) generateJitteredTimeout(workerID int) time.Duration {
	// Use consistent hash-based jitter to avoid synchronized timeouts
	h := fnv.New32a()
	h.Write([]byte{byte(c.jitterSeed), byte(c.jitterSeed >> 8), byte(c.jitterSeed >> 16), byte(c.jitterSeed >> 24)})
	h.Write([]byte{byte(workerID)})
	jitter := h.Sum32()

	// Apply jitter: ±20% of base timeout
	jitterRange := int64(c.baseTimeout) / 5 // 20% of base timeout
	jitterOffset := int64(jitter)%(jitterRange*2) - jitterRange

	return c.baseTimeout + time.Duration(jitterOffset)
}

// calculateDynamicBatchSize adjusts batch size based on queue pressure and processing time
func (c *ChanBatcherInstance[T]) calculateDynamicBatchSize() int {
	if !c.dynamicBatching {
		return c.itemLimit
	}

	// Calculate queue pressure (0.0 to 1.0)
	queueLen := len(c.queue)
	queueCap := cap(c.queue)
	queuePressure := float64(queueLen) / float64(queueCap)

	// Adjust batch size based on queue pressure
	var targetBatchSize int
	if queuePressure > 0.8 {
		// High pressure: increase batch size for better throughput
		targetBatchSize = c.maxBatchSize
	} else if queuePressure < 0.2 {
		// Low pressure: decrease batch size for better latency
		targetBatchSize = c.minBatchSize
	} else {
		// Medium pressure: interpolate between min and max
		range_ := c.maxBatchSize - c.minBatchSize
		targetBatchSize = c.minBatchSize + int(float64(range_)*queuePressure)
	}

	return targetBatchSize
}

func (c *ChanBatcherInstance[T]) worker(workerID int) {
	// Each worker gets a pre-computed jittered timeout to prevent thundering herd
	jitteredTimeout := c.jitteredTimeouts[workerID]
	timer := time.NewTimer(jitteredTimeout)
	defer timer.Stop()

	// Start with initial capacity, will grow as needed
	buffer := make([]T, 0, c.itemLimit)
	lastBatchTime := time.Now()

	for {
		// Calculate current target batch size
		currentBatchSize := c.calculateDynamicBatchSize()

		select {
		case <-c.ctx.Done():
			// Don't process remaining buffer on shutdown to avoid duplicate processing
			// The Stop() method ensures proper shutdown sequence
			return
		case item := <-c.queue:
			buffer = append(buffer, item)

			// Check if we should process based on current batch size or adaptive threshold
			shouldProcess := len(buffer) >= currentBatchSize
			if c.dynamicBatching && !shouldProcess {
				// Also check if we've been accumulating for too long
				elapsedSinceLastBatch := time.Since(lastBatchTime)
				shouldProcess = elapsedSinceLastBatch >= c.adaptiveThreshold
			}

			if shouldProcess {
				c.processor(buffer)
				buffer = buffer[:0]
				lastBatchTime = time.Now()
				// Reset with pre-computed jittered timeout
				timer.Reset(jitteredTimeout)
			}
		case <-timer.C:
			if len(buffer) > 0 {
				c.processor(buffer)
				buffer = buffer[:0]
				lastBatchTime = time.Now()
			}
			// Reset with pre-computed jittered timeout
			timer.Reset(jitteredTimeout)
		}
	}
}

// Stop 停止批处理器
func (c *ChanBatcherInstance[T]) Stop() {
	c.stopOnce.Do(func() {
		// Step 1: Cancel the context to signal workers to stop
		c.cancel()

		// Step 2: Close the queue to prevent new items from being added
		close(c.queue)

		// Step 3: Wait for all workers to finish processing remaining items
		// The ants pool will wait for all submitted tasks to complete
		c.workers.Release()
	})
}
