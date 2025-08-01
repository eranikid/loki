package metastore

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/grafana/dskit/backoff"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/thanos-io/objstore"

	"github.com/grafana/loki/v3/pkg/dataobj"
	"github.com/grafana/loki/v3/pkg/dataobj/consumer/logsobj"
	"github.com/grafana/loki/v3/pkg/dataobj/index/indexobj"
	"github.com/grafana/loki/v3/pkg/dataobj/sections/indexpointers"
	"github.com/grafana/loki/v3/pkg/dataobj/sections/streams"
	"github.com/grafana/loki/v3/pkg/logproto"
)

const (
	labelNameStart = "__start__"
	labelNameEnd   = "__end__"
	labelNamePath  = "__path__"
)

// StorageFormatType is the dataobj section type used to store the metastore top-level index oblects.
type StorageFormatType int

const (
	// StorageFormatTypeV1 is the old top-level streams based object format.
	StorageFormatTypeV1 StorageFormatType = iota
	// StorageFormatTypeV2 is the new top-level index pointer based object format.
	StorageFormatTypeV2
)

// Define our own builder config because metastore objects are significantly smaller.
var metastoreBuilderCfg = logsobj.BuilderConfig{
	TargetObjectSize:  32 * 1024 * 1024,
	TargetPageSize:    4 * 1024 * 1024,
	BufferSize:        32 * 1024 * 1024, // 8x page size
	TargetSectionSize: 4 * 1024 * 1024,  // object size / 8

	SectionStripeMergeLimit: 2,
}

type Updater struct {
	cfg              UpdaterConfig
	builder          *indexobj.Builder // New index pointer based builder.
	metastoreBuilder *logsobj.Builder  // Deprecated streams based builder.
	tenantID         string
	metrics          *metastoreMetrics
	bucket           objstore.Bucket
	logger           log.Logger
	backoff          *backoff.Backoff
	buf              *bytes.Buffer

	builderOnce sync.Once
}

func NewUpdater(cfg UpdaterConfig, bucket objstore.Bucket, tenantID string, logger log.Logger) *Updater {
	metrics := newMetastoreMetrics()

	return &Updater{
		cfg:      cfg,
		bucket:   bucket,
		metrics:  metrics,
		logger:   logger,
		tenantID: tenantID,
		backoff: backoff.New(context.TODO(), backoff.Config{
			MinBackoff: 50 * time.Millisecond,
			MaxBackoff: 10 * time.Second,
		}),
		builderOnce: sync.Once{},
	}
}

func (m *Updater) RegisterMetrics(reg prometheus.Registerer) error {
	return m.metrics.register(reg)
}

func (m *Updater) UnregisterMetrics(reg prometheus.Registerer) {
	m.metrics.unregister(reg)
}

func (m *Updater) initBuilder() error {
	var initErr error
	m.builderOnce.Do(func() {
		metastoreBuilder, err := logsobj.NewBuilder(metastoreBuilderCfg)
		if err != nil {
			initErr = err
			return
		}
		m.buf = bytes.NewBuffer(make([]byte, 0, metastoreBuilderCfg.TargetObjectSize))
		m.metastoreBuilder = metastoreBuilder

		indexBuilder, err := indexobj.NewBuilder(indexobj.BuilderConfig{
			TargetObjectSize:        metastoreBuilderCfg.TargetObjectSize,
			TargetPageSize:          metastoreBuilderCfg.TargetPageSize,
			BufferSize:              metastoreBuilderCfg.BufferSize,
			TargetSectionSize:       metastoreBuilderCfg.TargetSectionSize,
			SectionStripeMergeLimit: metastoreBuilderCfg.SectionStripeMergeLimit,
		})
		if err != nil {
			initErr = err
			return
		}
		m.builder = indexBuilder
	})
	return initErr
}

// Update adds provided dataobj path to the metastore. Flush stats are used to determine the stored metadata about this dataobj.
func (m *Updater) Update(ctx context.Context, dataobjPath string, minTimestamp, maxTimestamp time.Time) error {
	var err error
	processingTime := prometheus.NewTimer(m.metrics.metastoreProcessingTime)
	defer processingTime.ObserveDuration()

	// Initialize builder if this is the first call for this partition
	if err := m.initBuilder(); err != nil {
		return err
	}

	// Work our way through the metastore objects window by window, updating & creating them as needed.
	// Each one handles its own retries in order to keep making progress in the event of a failure.
	for metastorePath := range iterStorePaths(m.tenantID, minTimestamp, maxTimestamp) {
		m.backoff.Reset()
		for m.backoff.Ongoing() {
			err = m.bucket.GetAndReplace(ctx, metastorePath, func(existing io.ReadCloser) (io.ReadCloser, error) {
				if existing != nil {
					defer existing.Close()
				}

				m.buf.Reset()
				if existing != nil {
					level.Debug(m.logger).Log("msg", "found existing metastore, updating", "path", metastorePath)
					_, err := io.Copy(m.buf, existing)
					if err != nil {
						return nil, errors.Wrap(err, "copying to local buffer")
					}
				} else {
					level.Debug(m.logger).Log("msg", "no existing metastore found, creating new one", "path", metastorePath)
				}

				m.metastoreBuilder.Reset()
				m.builder.Reset()
				ty := m.cfg.StorageFormat

				if m.buf.Len() > 0 {
					replayDuration := prometheus.NewTimer(m.metrics.metastoreReplayTime)
					object, err := dataobj.FromReaderAt(bytes.NewReader(m.buf.Bytes()), int64(m.buf.Len()))
					if err != nil {
						return nil, errors.Wrap(err, "creating object from buffer")
					}
					ty, err = m.readFromExisting(ctx, object)
					if err != nil {
						return nil, errors.Wrap(err, "reading existing metastore version")
					}
					replayDuration.ObserveDuration()
				}

				encodingDuration := prometheus.NewTimer(m.metrics.metastoreEncodingTime)
				err = m.append(ty, dataobjPath, minTimestamp, maxTimestamp)
				if err != nil {
					return nil, errors.Wrap(err, "appending to metastore builder")
				}

				m.buf.Reset()

				switch ty {
				case StorageFormatTypeV1:
					_, err = m.metastoreBuilder.Flush(m.buf)
					if err != nil {
						return nil, errors.Wrap(err, "flushing metastore builder")
					}
				case StorageFormatTypeV2:
					_, err = m.builder.Flush(m.buf)
					if err != nil {
						return nil, errors.Wrap(err, "flushing metastore builder")
					}
				default:
					return nil, errors.New("unknown metastore top-level object type")
				}

				encodingDuration.ObserveDuration()
				return io.NopCloser(m.buf), nil
			})
			if err == nil {
				level.Info(m.logger).Log("msg", "successfully merged & updated metastore", "metastore", metastorePath)
				m.metrics.incMetastoreWrites(statusSuccess)
				break
			}
			level.Error(m.logger).Log("msg", "failed to get and replace metastore object", "err", err, "metastore", metastorePath)
			m.metrics.incMetastoreWrites(statusFailure)
			m.backoff.Wait()
		}
		// Reset at the end too so we don't leave our memory hanging around between calls.
		m.metastoreBuilder.Reset()
	}
	return err
}

func (m *Updater) append(ty StorageFormatType, dataobjPath string, minTimestamp, maxTimestamp time.Time) error {
	switch ty {
	// Backwards compatibility with old metastore top-level objects.
	case StorageFormatTypeV1:
		ls := labels.New(
			labels.Label{Name: labelNameStart, Value: strconv.FormatInt(minTimestamp.UnixNano(), 10)},
			labels.Label{Name: labelNameEnd, Value: strconv.FormatInt(maxTimestamp.UnixNano(), 10)},
			labels.Label{Name: labelNamePath, Value: dataobjPath},
		)

		err := m.metastoreBuilder.Append(logproto.Stream{
			Labels:  ls.String(),
			Entries: []logproto.Entry{{Line: ""}},
		})
		if err != nil {
			return errors.Wrap(err, "appending internal metadata stream")
		}
	// New standard approach for metastore top-level objects.
	case StorageFormatTypeV2:
		err := m.builder.AppendIndexPointer(dataobjPath, minTimestamp, maxTimestamp)
		if err != nil {
			return errors.Wrap(err, "appending index pointer")
		}
	default:
		return errors.New("unknown metastore top-level object type")
	}

	return nil
}

// readFromExisting reads the provided metastore object and appends the streams to the builder so it can be later modified.
func (m *Updater) readFromExisting(ctx context.Context, object *dataobj.Object) (StorageFormatType, error) {
	var streamsReader streams.RowReader
	defer streamsReader.Close()

	var indexPointersReader indexpointers.RowReader
	defer indexPointersReader.Close()

	// Read streams from existing metastore object and write them to the builder for the new object
	buf := make([]streams.Stream, 100)
	pbuf := make([]indexpointers.IndexPointer, 100)

	for _, section := range object.Sections() {
		if !streams.CheckSection(section) && !indexpointers.CheckSection(section) {
			continue
		}

		switch {
		// Backwards compatibility with old metastore top-level objects.
		case streams.CheckSection(section):
			sec, err := streams.Open(ctx, section)
			if err != nil {
				return StorageFormatTypeV1, errors.Wrap(err, "opening section")
			}

			streamsReader.Reset(sec)
			for n, err := streamsReader.Read(ctx, buf); n > 0; n, err = streamsReader.Read(ctx, buf) {
				if err != nil && err != io.EOF {
					return StorageFormatTypeV1, errors.Wrap(err, "reading streams")
				}
				for _, stream := range buf[:n] {
					err = m.metastoreBuilder.Append(logproto.Stream{
						Labels:  stream.Labels.String(),
						Entries: []logproto.Entry{{Line: ""}},
					})
					if err != nil {
						return StorageFormatTypeV1, errors.Wrap(err, "appending streams")
					}
				}
			}

			return StorageFormatTypeV1, nil
		// New standard approach for metastore top-level objects.
		case indexpointers.CheckSection(section):
			sec, err := indexpointers.Open(ctx, section)
			if err != nil {
				return StorageFormatTypeV2, errors.Wrap(err, "opening section")
			}
			indexPointersReader.Reset(sec)
			for n, err := indexPointersReader.Read(ctx, pbuf); n > 0; n, err = indexPointersReader.Read(ctx, pbuf) {
				if err != nil && err != io.EOF {
					return StorageFormatTypeV2, errors.Wrap(err, "reading index pointers")
				}
				for _, indexPointer := range pbuf[:n] {
					err = m.builder.AppendIndexPointer(indexPointer.Path, indexPointer.StartTs, indexPointer.EndTs)
					if err != nil {
						return StorageFormatTypeV2, errors.Wrap(err, "appending index pointers")
					}
				}
			}

			return StorageFormatTypeV2, nil
		}
	}

	return m.cfg.StorageFormat, nil
}
