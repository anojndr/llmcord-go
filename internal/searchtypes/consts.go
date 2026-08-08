// Package searchtypes holds search metadata and message part keys.
package searchtypes

// Message part keys shared by every provider-facing message payload.
const (
	MessageRoleUser      = "user"
	MessageRoleSystem    = "system"
	MessageRoleAssistant = "assistant"
	MessageContentKey    = "content"
	MessageRoleKey       = "role"
	MessageTypeKey       = "type"
	MessageTextKey       = "text"
	MessageURLKey        = "url"
	MessageDetailKey     = "detail"
	MessageKindValue     = "message"
)

// Content part type and field keys carried inside multimodal messages.
const (
	ContentTypeAudioData = "audio_data"
	ContentTypeDocument  = "document_data"
	ContentTypeFileData  = "file_data"
	ContentTypeImageURL  = "image_url"
	ContentTypeText      = "text"
	ContentTypeVideoData = "video_data"
	ContentFieldBytes    = "data"
	ContentFieldFilename = "filename"
	ContentFieldMIMEType = "mime_type"
)

// MIME types used to classify attachments and documents.
const (
	MimeTypeDOCX        = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	MimeTypeOctetStream = "application/octet-stream"
	MimeTypePDF         = "application/pdf"
	MimeTypePPTX        = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	MimeTypeJPEG        = "image/jpeg"
	MimeTypePNG         = "image/png"
	MimeTypeZIP         = "application/zip"
	MimeTypeWEBP        = "image/webp"
)
