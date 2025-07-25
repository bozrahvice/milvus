package adaptor

import (
	"go.uber.org/zap"

	"github.com/milvus-io/milvus/pkg/v2/log"
	"github.com/milvus-io/milvus/pkg/v2/mq/msgstream"
	"github.com/milvus-io/milvus/pkg/v2/streaming/util/message"
	"github.com/milvus-io/milvus/pkg/v2/util/typeutil"
)

type ChanMessageHandler chan message.ImmutableMessage

func (h ChanMessageHandler) Handle(param message.HandleParam) message.HandleResult {
	var sendingCh chan message.ImmutableMessage
	if param.Message != nil {
		sendingCh = h
	}
	select {
	case <-param.Ctx.Done():
		return message.HandleResult{Error: param.Ctx.Err()}
	case msg, ok := <-param.Upstream:
		if !ok {
			panic("unreachable code: upstream should never closed")
		}
		return message.HandleResult{Incoming: msg}
	case sendingCh <- param.Message:
		return message.HandleResult{MessageHandled: true}
	}
}

func (d ChanMessageHandler) Close() {
	close(d)
}

// NewMsgPackAdaptorHandler create a new message pack adaptor handler.
func NewMsgPackAdaptorHandler() *MsgPackAdaptorHandler {
	return &MsgPackAdaptorHandler{
		channel: make(chan *msgstream.ConsumeMsgPack),
		base:    NewBaseMsgPackAdaptorHandler(),
	}
}

type MsgPackAdaptorHandler struct {
	channel chan *msgstream.ConsumeMsgPack
	base    *BaseMsgPackAdaptorHandler
}

// Chan is the channel for message.
func (m *MsgPackAdaptorHandler) Chan() <-chan *msgstream.ConsumeMsgPack {
	return m.channel
}

// Handle is the callback for handling message.
func (m *MsgPackAdaptorHandler) Handle(param message.HandleParam) message.HandleResult {
	messageHandled := false
	// not handle new message if there are pending msgPack.
	if param.Message != nil && m.base.PendingMsgPack.Len() == 0 {
		m.base.GenerateMsgPack(param.Message)
		messageHandled = true
	}

	for {
		var sendCh chan<- *msgstream.ConsumeMsgPack
		if m.base.PendingMsgPack.Len() != 0 {
			sendCh = m.channel
		}

		// If there's no pending msgPack and no upstream message,
		// return it immediately to ask for more message from upstream to avoid blocking.
		if sendCh == nil && param.Upstream == nil {
			return message.HandleResult{
				MessageHandled: messageHandled,
			}
		}
		select {
		case <-param.Ctx.Done():
			return message.HandleResult{
				MessageHandled: messageHandled,
				Error:          param.Ctx.Err(),
			}
		case msg, ok := <-param.Upstream:
			if !ok {
				panic("unreachable code: upstream should never closed")
			}
			return message.HandleResult{
				Incoming:       msg,
				MessageHandled: messageHandled,
			}
		case sendCh <- m.base.PendingMsgPack.Next():
			m.base.PendingMsgPack.UnsafeAdvance()
			if m.base.PendingMsgPack.Len() > 0 {
				continue
			}
			return message.HandleResult{MessageHandled: messageHandled}
		}
	}
}

// Close closes the handler.
func (m *MsgPackAdaptorHandler) Close() {
	close(m.channel)
}

// NewBaseMsgPackAdaptorHandler create a new base message pack adaptor handler.
func NewBaseMsgPackAdaptorHandler() *BaseMsgPackAdaptorHandler {
	return &BaseMsgPackAdaptorHandler{
		Logger:         log.With(),
		Pendings:       make([]message.ImmutableMessage, 0),
		PendingMsgPack: typeutil.NewMultipartQueue[*msgstream.ConsumeMsgPack](),
	}
}

// BaseMsgPackAdaptorHandler is the handler for message pack.
type BaseMsgPackAdaptorHandler struct {
	Logger         *log.MLogger
	Pendings       []message.ImmutableMessage                          // pendings hold the vOld message which has same time tick.
	PendingMsgPack *typeutil.MultipartQueue[*msgstream.ConsumeMsgPack] // pendingMsgPack hold unsent msgPack.
}

// GenerateMsgPack generate msgPack from message.
func (m *BaseMsgPackAdaptorHandler) GenerateMsgPack(msg message.ImmutableMessage) {
	switch msg.Version() {
	case message.VersionOld:
		if len(m.Pendings) != 0 {
			// multiple message from old version may share the same time tick.
			// should be packed into one msgPack.
			if msg.TimeTick() > m.Pendings[0].TimeTick() {
				m.addMsgPackIntoPending(m.Pendings...)
				m.Pendings = nil
			} else if msg.TimeTick() < m.Pendings[0].TimeTick() {
				m.Logger.Warn("message time tick is less than pendings",
					zap.String("messageID", msg.MessageID().String()),
					zap.String("pendingMessageID", m.Pendings[0].MessageID().String()),
					zap.Uint64("timeTick", msg.TimeTick()),
					zap.Uint64("pendingTimeTick", m.Pendings[0].TimeTick()))
				return
			}
		}
		m.Pendings = append(m.Pendings, msg)
	case message.VersionV1, message.VersionV2:
		if len(m.Pendings) != 0 { // all previous message should be vOld.
			m.addMsgPackIntoPending(m.Pendings...)
			m.Pendings = nil
		}
		m.addMsgPackIntoPending(msg)
	default:
		panic("unsupported message version")
	}
}

// addMsgPackIntoPending add message into pending msgPack.
func (m *BaseMsgPackAdaptorHandler) addMsgPackIntoPending(msgs ...message.ImmutableMessage) {
	// Because the old version message may have same time tick,
	// So we may read the same message multiple times on same time tick because of the auto-resuming by ResumableConsumer.
	// we need to filter out the duplicate messages here.
	dedupMessages := make([]message.ImmutableMessage, 0, len(msgs))
	for _, msg := range msgs {
		exist := false
		for _, existMsg := range dedupMessages {
			if msg.MessageID().EQ(existMsg.MessageID()) {
				exist = true
				break
			}
		}
		if !exist {
			dedupMessages = append(dedupMessages, msg)
		}
	}
	newPack, err := NewMsgPackFromMessage(dedupMessages...)
	if err != nil {
		m.Logger.Warn("failed to convert message to msgpack", zap.Error(err))
	}
	if newPack != nil {
		m.PendingMsgPack.AddOne(msgstream.BuildConsumeMsgPack(newPack))
	}
}
