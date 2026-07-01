package octo

import "github.com/multica-ai/multica/server/internal/integrations/channel"

// TypeOcto is the channel discriminator for the Octo IM adapter.
const TypeOcto channel.Type = "octo"

type octoChannelType int

const (
	octoChannelTypeDM             octoChannelType = 1
	octoChannelTypeGroup          octoChannelType = 2
	octoChannelTypeCommunityTopic octoChannelType = 5
)

const (
	octoMessageTypeText            = 1
	octoMessageTypeImage           = 2
	octoMessageTypeGIF             = 3
	octoMessageTypeVoice           = 4
	octoMessageTypeVideo           = 5
	octoMessageTypeFile            = 8
	octoMessageTypeMultipleForward = 11
	octoMessageTypeRichText        = 14
)

const (
	richTextBlockText             = "text"
	richTextBlockImage            = "image"
	richTextImagePlaceholder      = "[图片]"
	octoUnknownFileName           = "未知文件"
	octoMultipleForwardText       = "[合并转发]"
	octoMultipleForwardRecordText = "[合并转发: 聊天记录]"
)

type botMessage struct {
	MessageID   string          `json:"message_id"`
	MessageSeq  int32           `json:"message_seq"`
	FromUID     string          `json:"from_uid"`
	ChannelID   string          `json:"channel_id,omitempty"`
	ChannelType octoChannelType `json:"channel_type,omitempty"`
	Timestamp   int64           `json:"timestamp"`
	Payload     messagePayload  `json:"payload"`
}

type messagePayload struct {
	Type    int             `json:"type"`
	Content any             `json:"content,omitempty"`
	URL     string          `json:"url,omitempty"`
	Name    string          `json:"name,omitempty"`
	Mention *mentionPayload `json:"mention,omitempty"`
	Reply   *replyPayload   `json:"reply,omitempty"`
	Plain   string          `json:"plain,omitempty"`
	Users   []forwardUser   `json:"users,omitempty"`
	Msgs    []forwardMsg    `json:"msgs,omitempty"`
}

type replyPayload struct {
	MessageID string          `json:"message_id,omitempty"`
	Payload   *messagePayload `json:"payload,omitempty"`
	FromUID   string          `json:"from_uid,omitempty"`
	FromName  string          `json:"from_name,omitempty"`
}

type mentionPayload struct {
	UIDs     []string        `json:"uids,omitempty"`
	Entities []mentionEntity `json:"entities,omitempty"`
	All      any             `json:"all,omitempty"`
	Humans   any             `json:"humans,omitempty"`
	AIs      any             `json:"ais,omitempty"`
}

type mentionEntity struct {
	UID    string `json:"uid"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
}

type richTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	URL  string `json:"url,omitempty"`
	Name string `json:"name,omitempty"`
}

type forwardUser struct {
	UID  string `json:"uid"`
	Name string `json:"name"`
}

type forwardMsg struct {
	FromUID string         `json:"from_uid"`
	Payload messagePayload `json:"payload"`
}

type octoRawMessage struct {
	RobotID     string          `json:"robot_id"`
	ChannelType octoChannelType `json:"channel_type"`
	MessageSeq  int32           `json:"message_seq"`
	Timestamp   int64           `json:"timestamp"`
}
