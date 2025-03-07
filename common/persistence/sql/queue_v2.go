// The MIT License
//
// Copyright (c) 2020 Temporal Technologies Inc.  All rights reserved.
//
// Copyright (c) 2020 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package sql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	commonpb "go.temporal.io/api/common/v1"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"

	persistencespb "go.temporal.io/server/api/persistence/v1"
	"go.temporal.io/server/common/log"
	"go.temporal.io/server/common/log/tag"
	"go.temporal.io/server/common/persistence"
	"go.temporal.io/server/common/persistence/serialization"
	"go.temporal.io/server/common/persistence/sql/sqlplugin"
)

const (
	defaultPartition = 0
)

type (
	queueV2 struct {
		SqlStore
	}

	QueueV2Metadata struct {
		Metadata *persistencespb.Queue
		Version  int64
	}
)

// NewQueueV2 returns an implementation of persistence.QueueV2.
func NewQueueV2(db sqlplugin.DB,
	logger log.Logger,
) persistence.QueueV2 {
	return &queueV2{
		SqlStore: NewSqlStore(db, logger),
	}
}

func (q *queueV2) EnqueueMessage(
	ctx context.Context,
	request *persistence.InternalEnqueueMessageRequest,
) (*persistence.InternalEnqueueMessageResponse, error) {

	_, err := q.getQueueMetadata(ctx, q.Db, request.QueueType, request.QueueName)
	if err != nil {
		return nil, err
	}
	tx, err := q.Db.BeginTx(ctx)
	if err != nil {
		return nil, serviceerror.NewUnavailable(fmt.Sprintf(
			"EnqueueMessage failed for queue with type: %v and name: %v. BeginTx operation failed. Error: %v",
			request.QueueType,
			request.QueueName,
			err),
		)
	}
	nextMessageID, err := q.getNextMessageID(ctx, request.QueueType, request.QueueName, tx)
	if err != nil {
		rollBackErr := tx.Rollback()
		if rollBackErr != nil {
			q.SqlStore.logger.Error("transaction rollback error", tag.Error(rollBackErr))
		}
		return nil, serviceerror.NewUnavailable(fmt.Sprintf(
			"EnqueueMessage failed for queue with type: %v and name: %v. failed to get next messageId. Error: %v",
			request.QueueType,
			request.QueueName,
			err),
		)
	}
	_, err = tx.InsertIntoQueueV2Messages(ctx, []sqlplugin.QueueV2MessageRow{
		newQueueV2Row(request.QueueType, request.QueueName, nextMessageID, request.Blob),
	})
	if err != nil {
		rollBackErr := tx.Rollback()
		if rollBackErr != nil {
			q.SqlStore.logger.Error("transaction rollback error", tag.Error(rollBackErr))
		}
		return nil, serviceerror.NewUnavailable(fmt.Sprintf(
			"EnqueueMessage failed for queue with type: %v and name: %v. InsertIntoQueueV2Messages operation failed. Error: %v",
			request.QueueType,
			request.QueueName,
			err),
		)
	}

	if err := tx.Commit(); err != nil {
		return nil, serviceerror.NewUnavailable(fmt.Sprintf(
			"EnqueueMessage failed for queue with type: %v and name: %v. commit operation failed. Error: %v",
			request.QueueType,
			request.QueueName,
			err),
		)
	}
	return &persistence.InternalEnqueueMessageResponse{Metadata: persistence.MessageMetadata{ID: nextMessageID}}, err
}

func (q *queueV2) ReadMessages(
	ctx context.Context,
	request *persistence.InternalReadMessagesRequest,
) (*persistence.InternalReadMessagesResponse, error) {

	if request.PageSize <= 0 {
		return nil, persistence.ErrNonPositiveReadQueueMessagesPageSize
	}
	qm, err := q.getQueueMetadata(ctx, q.Db, request.QueueType, request.QueueName)
	if err != nil {
		return nil, err
	}
	minMessageID, err := persistence.GetMinMessageIDToReadForQueueV2(
		request.QueueType,
		request.QueueName,
		request.NextPageToken,
		qm.Metadata,
	)
	if err != nil {
		return nil, err
	}
	rows, err := q.Db.RangeSelectFromQueueV2Messages(ctx, sqlplugin.QueueV2MessagesFilter{
		QueueType:    request.QueueType,
		QueueName:    request.QueueName,
		Partition:    defaultPartition,
		MinMessageID: minMessageID,
		PageSize:     request.PageSize,
	})
	if err != nil {
		return nil, serviceerror.NewUnavailable(fmt.Sprintf(
			"ReadMessages failed for queue with type: %v and name: %v. RangeSelectFromQueueV2Messages operation failed. Error: %v",
			request.QueueType,
			request.QueueName,
			err),
		)
	}
	var messages []persistence.QueueV2Message
	for _, row := range rows {
		encoding, ok := enums.EncodingType_value[row.MessageEncoding]
		if !ok {
			return nil, serialization.NewUnknownEncodingTypeError(row.MessageEncoding)
		}
		encodingType := enums.EncodingType(encoding)
		message := persistence.QueueV2Message{
			MetaData: persistence.MessageMetadata{ID: row.MessageID},
			Data: commonpb.DataBlob{
				EncodingType: encodingType,
				Data:         row.MessagePayload,
			},
		}
		messages = append(messages, message)
	}
	nextPageToken := persistence.GetNextPageTokenForQueueV2(messages)
	response := &persistence.InternalReadMessagesResponse{
		Messages:      messages,
		NextPageToken: nextPageToken,
	}
	return response, nil
}

func newQueueV2Row(
	queueType persistence.QueueV2Type,
	queueName string,
	messageID int64,
	blob commonpb.DataBlob,
) sqlplugin.QueueV2MessageRow {

	return sqlplugin.QueueV2MessageRow{
		QueueType:       queueType,
		QueueName:       queueName,
		QueuePartition:  defaultPartition,
		MessageID:       messageID,
		MessagePayload:  blob.Data,
		MessageEncoding: blob.EncodingType.String(),
	}
}

func (q *queueV2) CreateQueue(
	ctx context.Context,
	request *persistence.InternalCreateQueueRequest,
) (*persistence.InternalCreateQueueResponse, error) {
	payload := persistencespb.Queue{
		Partitions: map[int32]*persistencespb.QueuePartition{
			defaultPartition: {
				MinMessageId: persistence.FirstQueueMessageID,
			},
		},
	}
	bytes, _ := payload.Marshal()
	row := sqlplugin.QueueV2MetadataRow{
		QueueType:        request.QueueType,
		QueueName:        request.QueueName,
		MetadataPayload:  bytes,
		MetadataEncoding: enums.ENCODING_TYPE_PROTO3.String(),
		Version:          0,
	}
	_, err := q.Db.InsertIntoQueueV2Metadata(ctx, &row)
	if q.Db.IsDupEntryError(err) {
		return nil, fmt.Errorf(
			"%w: queue type %v and name %v",
			persistence.ErrQueueAlreadyExists,
			request.QueueType,
			request.QueueName,
		)
	}
	if err != nil {
		return nil, serviceerror.NewUnavailable(fmt.Sprintf(
			"ReadMessages failed for queue with type: %v and name: %v. InsertIntoQueueV2Metadata operation failed. Error: %v",
			request.QueueType,
			request.QueueName,
			err),
		)
	}
	return &persistence.InternalCreateQueueResponse{}, nil
}

func (q *queueV2) RangeDeleteMessages(
	ctx context.Context,
	request *persistence.InternalRangeDeleteMessagesRequest,
) (*persistence.InternalRangeDeleteMessagesResponse, error) {
	if request.InclusiveMaxMessageMetadata.ID < persistence.FirstQueueMessageID {
		return nil, fmt.Errorf(
			"%w: id is %d but must be >= %d",
			persistence.ErrInvalidQueueRangeDeleteMaxMessageID,
			request.InclusiveMaxMessageMetadata.ID,
			persistence.FirstQueueMessageID,
		)
	}
	err := q.txExecute(ctx, "RangeDeleteMessages", func(tx sqlplugin.Tx) error {
		qm, err := q.getQueueMetadata(ctx, tx, request.QueueType, request.QueueName)
		if err != nil {
			return err
		}
		partition, err := persistence.GetPartitionForQueueV2(request.QueueType, request.QueueName, qm.Metadata)
		if err != nil {
			return err
		}
		maxMessageID, ok, err := q.getMaxMessageID(ctx, request.QueueType, request.QueueName, tx)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		deleteRange, ok := persistence.GetDeleteRange(persistence.DeleteRequest{
			LastIDToDeleteInclusive: request.InclusiveMaxMessageMetadata.ID,
			ExistingMessageRange: persistence.InclusiveMessageRange{
				MinMessageID: partition.MinMessageId,
				MaxMessageID: maxMessageID,
			},
		})
		if !ok {
			return nil
		}
		msgFilter := sqlplugin.QueueV2MessagesFilter{
			QueueType:    request.QueueType,
			QueueName:    request.QueueName,
			Partition:    defaultPartition,
			MinMessageID: deleteRange.MinMessageID,
			MaxMessageID: deleteRange.MaxMessageID,
		}
		_, err = tx.RangeDeleteFromQueueV2Messages(ctx, msgFilter)
		if err != nil {
			return err
		}
		partition.MinMessageId = deleteRange.NewMinMessageID
		bytes, _ := qm.Metadata.Marshal()
		row := sqlplugin.QueueV2MetadataRow{
			QueueType:        request.QueueType,
			QueueName:        request.QueueName,
			MetadataPayload:  bytes,
			MetadataEncoding: enums.ENCODING_TYPE_PROTO3.String(),
			Version:          0,
		}
		_, err = tx.UpdateQueueV2Metadata(ctx, &row)
		if err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &persistence.InternalRangeDeleteMessagesResponse{}, nil
}

func (q *queueV2) getQueueMetadata(
	ctx context.Context,
	tc sqlplugin.TableCRUD,
	queueType persistence.QueueV2Type,
	queueName string,
) (*QueueV2Metadata, error) {

	filter := sqlplugin.QueueV2MetadataFilter{
		QueueType: queueType,
		QueueName: queueName,
	}
	metadata, err := tc.SelectFromQueueV2Metadata(ctx, filter)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, persistence.NewQueueNotFoundError(queueType, queueName)
		}
		return nil, serviceerror.NewUnavailable(
			fmt.Sprintf("failed to get metadata for queue with type: %v and name: %v. Error: %v", queueType, queueName, err),
		)
	}
	if metadata.MetadataEncoding != enums.ENCODING_TYPE_PROTO3.String() {
		return nil, fmt.Errorf(
			"queue with type %v and name %v has invalid encoding: %w",
			metadata.QueueType,
			metadata.QueueName,
			serialization.NewUnknownEncodingTypeError(metadata.MetadataEncoding, enums.ENCODING_TYPE_PROTO3),
		)
	}
	qm := &persistencespb.Queue{}
	err = qm.Unmarshal(metadata.MetadataPayload)
	if err != nil {
		return nil, serialization.NewDeserializationError(
			enums.ENCODING_TYPE_PROTO3,
			fmt.Errorf("unmarshal payload for queue with type %v and name %v failed: %w",
				metadata.QueueType,
				metadata.QueueName,
				err),
		)
	}
	return &QueueV2Metadata{
		Metadata: qm,
		Version:  metadata.Version,
	}, nil
}

func (q *queueV2) getMaxMessageID(ctx context.Context, queueType persistence.QueueV2Type, queueName string, tx sqlplugin.Tx) (int64, bool, error) {
	lastMessageID, err := tx.GetLastEnqueuedMessageIDForUpdateV2(ctx, sqlplugin.QueueV2Filter{
		QueueType: queueType,
		QueueName: queueName,
		Partition: defaultPartition,
	})
	switch {
	case err == nil:
		return lastMessageID, true, nil
	case errors.Is(err, sql.ErrNoRows):
		return 0, false, nil
	default:
		return 0, false, err
	}
}

func (q *queueV2) getNextMessageID(ctx context.Context, queueType persistence.QueueV2Type, queueName string, tx sqlplugin.Tx) (int64, error) {
	maxMessageID, ok, err := q.getMaxMessageID(ctx, queueType, queueName, tx)
	if err != nil {
		return 0, err
	}
	if !ok {
		return persistence.FirstQueueMessageID, nil
	}
	return maxMessageID + 1, nil
}
