package cosmos

import (
	"encoding/hex"
	"strconv"
	"strings"

	sdk "github.com/cosmos/cosmos-sdk/types"
	clienttypes "github.com/cosmos/ibc-go/v3/modules/core/02-client/types"
	conntypes "github.com/cosmos/ibc-go/v3/modules/core/03-connection/types"
	chantypes "github.com/cosmos/ibc-go/v3/modules/core/04-channel/types"
	"github.com/cosmos/relayer/v2/relayer/provider"
	abci "github.com/tendermint/tendermint/abci/types"
	"go.uber.org/zap"
)

// ibcMessage is the type used for parsing all possible properties of IBC messages
type ibcMessage struct {
	action string
	info   ibcMessageInfo
}

type ibcMessageInfo interface {
	parseAttrs(log *zap.Logger, attrs []sdk.Attribute)
}

// ibcMessagesFromTransaction parses all events within a transaction to find IBC messages
func (ccp *CosmosChainProcessor) ibcMessagesFromTransaction(tx *abci.ResponseDeliverTx, height uint64) []ibcMessage {
	parsedLogs, err := sdk.ParseABCILogs(tx.Log)
	if err != nil {
		ccp.log.Info("Failed to parse abci logs", zap.Error(err))
		return nil
	}
	return parseABCILogs(ccp.log, parsedLogs, height)
}

func parseABCILogs(log *zap.Logger, logs sdk.ABCIMessageLogs, height uint64) (messages []ibcMessage) {
	for _, messageLog := range logs {
		var info ibcMessageInfo
		var action string
		var packetAccumulator *packetInfo
		for _, event := range messageLog.Events {
			switch event.Type {
			case "message":
				for _, attr := range event.Attributes {
					switch attr.Key {
					case "action":
						action = attr.Value
					}
				}
			case clienttypes.EventTypeCreateClient, clienttypes.EventTypeUpdateClient,
				clienttypes.EventTypeUpgradeClient, clienttypes.EventTypeSubmitMisbehaviour,
				clienttypes.EventTypeUpdateClientProposal:
				clientInfo := new(clientInfo)
				clientInfo.parseAttrs(log, event.Attributes)
				info = clientInfo
			case chantypes.EventTypeSendPacket, chantypes.EventTypeRecvPacket,
				chantypes.EventTypeAcknowledgePacket, chantypes.EventTypeTimeoutPacket,
				chantypes.EventTypeTimeoutPacketOnClose, chantypes.EventTypeWriteAck:
				if packetAccumulator == nil {
					packetAccumulator = &packetInfo{Height: height}
				}
				packetAccumulator.parseAttrs(log, event.Attributes)
				info = packetAccumulator
			case conntypes.EventTypeConnectionOpenInit, conntypes.EventTypeConnectionOpenTry,
				conntypes.EventTypeConnectionOpenAck, conntypes.EventTypeConnectionOpenConfirm:
				connectionInfo := &connectionInfo{Height: height}
				connectionInfo.parseAttrs(log, event.Attributes)
				info = connectionInfo
			case chantypes.EventTypeChannelOpenInit, chantypes.EventTypeChannelOpenTry,
				chantypes.EventTypeChannelOpenAck, chantypes.EventTypeChannelOpenConfirm,
				chantypes.EventTypeChannelCloseInit, chantypes.EventTypeChannelCloseConfirm:
				channelInfo := &channelInfo{Height: height}
				channelInfo.parseAttrs(log, event.Attributes)
				info = channelInfo
			}
		}

		if info == nil {
			// Not an IBC message, don't need to log here
			continue
		}
		if action == "" {
			log.Error("Unexpected ibc message parser state: message info is populated but action is empty",
				zap.Any("info", info),
			)
			continue
		}
		messages = append(messages, ibcMessage{
			action: action,
			info:   info,
		})
	}

	return messages
}

// clientInfo contains the consensus height of the counterparty chain for a client.
type clientInfo struct {
	clientID        string
	consensusHeight clienttypes.Height
	header          []byte
}

func (c clientInfo) ClientState() provider.ClientState {
	return provider.ClientState{
		ClientID:        c.clientID,
		ConsensusHeight: c.consensusHeight,
	}
}

func (res *clientInfo) parseAttrs(log *zap.Logger, attributes []sdk.Attribute) {
	for _, attr := range attributes {
		res.parseClientAttribute(log, attr)
	}
}

func (res *clientInfo) parseClientAttribute(log *zap.Logger, attr sdk.Attribute) {
	switch attr.Key {
	case clienttypes.AttributeKeyClientID:
		res.clientID = attr.Value
	case clienttypes.AttributeKeyConsensusHeight:
		revisionSplit := strings.Split(attr.Value, "-")
		if len(revisionSplit) != 2 {
			log.Error("Error parsing client consensus height",
				zap.String("client_id", res.clientID),
				zap.String("value", attr.Value),
			)
			return
		}
		revisionNumberString := revisionSplit[0]
		revisionNumber, err := strconv.ParseUint(revisionNumberString, 10, 64)
		if err != nil {
			log.Error("Error parsing client consensus height revision number",
				zap.Error(err),
			)
			return
		}
		revisionHeightString := revisionSplit[1]
		revisionHeight, err := strconv.ParseUint(revisionHeightString, 10, 64)
		if err != nil {
			log.Error("Error parsing client consensus height revision height",
				zap.Error(err),
			)
			return
		}
		res.consensusHeight = clienttypes.Height{
			RevisionNumber: revisionNumber,
			RevisionHeight: revisionHeight,
		}
	case clienttypes.AttributeKeyHeader:
		data, err := hex.DecodeString(attr.Value)
		if err != nil {
			log.Error("Error parsing client header",
				zap.String("header", attr.Value),
				zap.Error(err),
			)
			return
		}
		res.header = data
	}
}

// alias type to the provider types, used for adding parser methods
type packetInfo provider.PacketInfo

// parsePacketInfo is treated differently from the others since it can be constructed from the accumulation of multiple events
func (res *packetInfo) parseAttrs(log *zap.Logger, attrs []sdk.Attribute) {
	for _, attr := range attrs {
		res.parsePacketAttribute(log, attr)
	}
}

func (res *packetInfo) parsePacketAttribute(log *zap.Logger, attr sdk.Attribute) {
	var err error
	switch attr.Key {
	case chantypes.AttributeKeySequence:
		res.Sequence, err = strconv.ParseUint(attr.Value, 10, 64)
		if err != nil {
			log.Error("Error parsing packet sequence",
				zap.String("value", attr.Value),
				zap.Error(err),
			)
			return
		}
	case chantypes.AttributeKeyTimeoutTimestamp:
		res.TimeoutTimestamp, err = strconv.ParseUint(attr.Value, 10, 64)
		if err != nil {
			log.Error("Error parsing packet timestamp",
				zap.Uint64("sequence", res.Sequence),
				zap.String("value", attr.Value),
				zap.Error(err),
			)
			return
		}
	// NOTE: deprecated per IBC spec
	case chantypes.AttributeKeyData:
		res.Data = []byte(attr.Value)
	case chantypes.AttributeKeyDataHex:
		data, err := hex.DecodeString(attr.Value)
		if err != nil {
			log.Error("Error parsing packet data",
				zap.Uint64("sequence", res.Sequence),
				zap.Error(err),
			)
			return
		}
		res.Data = data
	// NOTE: deprecated per IBC spec
	case chantypes.AttributeKeyAck:
		res.Ack = []byte(attr.Value)
	case chantypes.AttributeKeyAckHex:
		data, err := hex.DecodeString(attr.Value)
		if err != nil {
			log.Error("Error parsing packet ack",
				zap.Uint64("sequence", res.Sequence),
				zap.String("value", attr.Value),
				zap.Error(err),
			)
			return
		}
		res.Ack = data
	case chantypes.AttributeKeyTimeoutHeight:
		timeoutSplit := strings.Split(attr.Value, "-")
		if len(timeoutSplit) != 2 {
			log.Error("Error parsing packet height timeout",
				zap.Uint64("sequence", res.Sequence),
				zap.String("value", attr.Value),
			)
			return
		}
		revisionNumber, err := strconv.ParseUint(timeoutSplit[0], 10, 64)
		if err != nil {
			log.Error("Error parsing packet timeout height revision number",
				zap.Uint64("sequence", res.Sequence),
				zap.String("value", timeoutSplit[0]),
				zap.Error(err),
			)
			return
		}
		revisionHeight, err := strconv.ParseUint(timeoutSplit[1], 10, 64)
		if err != nil {
			log.Error("Error parsing packet timeout height revision height",
				zap.Uint64("sequence", res.Sequence),
				zap.String("value", timeoutSplit[1]),
				zap.Error(err),
			)
			return
		}
		res.TimeoutHeight = clienttypes.Height{
			RevisionNumber: revisionNumber,
			RevisionHeight: revisionHeight,
		}
	case chantypes.AttributeKeySrcPort:
		res.SourcePort = attr.Value
	case chantypes.AttributeKeySrcChannel:
		res.SourceChannel = attr.Value
	case chantypes.AttributeKeyDstPort:
		res.DestPort = attr.Value
	case chantypes.AttributeKeyDstChannel:
		res.DestChannel = attr.Value
	}
}

// alias type to the provider types, used for adding parser methods
type channelInfo provider.ChannelInfo

func (res *channelInfo) parseAttrs(log *zap.Logger, attrs []sdk.Attribute) {
	for _, attr := range attrs {
		res.parseChannelAttribute(attr)
	}
}

func (res *channelInfo) parseChannelAttribute(attr sdk.Attribute) {
	switch attr.Key {
	case chantypes.AttributeKeyPortID:
		res.PortID = attr.Value
	case chantypes.AttributeKeyChannelID:
		res.ChannelID = attr.Value
	case chantypes.AttributeCounterpartyPortID:
		res.CounterpartyPortID = attr.Value
	case chantypes.AttributeCounterpartyChannelID:
		res.CounterpartyChannelID = attr.Value
	case chantypes.AttributeKeyConnectionID:
		res.ConnID = attr.Value
	}
}

// alias type to the provider types, used for adding parser methods
type connectionInfo provider.ConnectionInfo

func (res *connectionInfo) parseAttrs(log *zap.Logger, attrs []sdk.Attribute) {
	for _, attr := range attrs {
		res.parseConnectionAttribute(attr)
	}
}

func (res *connectionInfo) parseConnectionAttribute(attr sdk.Attribute) {
	switch attr.Key {
	case conntypes.AttributeKeyConnectionID:
		res.ConnID = attr.Value
	case conntypes.AttributeKeyClientID:
		res.ClientID = attr.Value
	case conntypes.AttributeKeyCounterpartyConnectionID:
		res.CounterpartyConnID = attr.Value
	case conntypes.AttributeKeyCounterpartyClientID:
		res.CounterpartyClientID = attr.Value
	}
}
