package webex

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/prometheus/alertmanager/template"
	"github.com/prometheus/alertmanager/types"

	"github.com/grafana/alerting/images"
	"github.com/grafana/alerting/logging"
	"github.com/grafana/alerting/receivers"
	template2 "github.com/grafana/alerting/templates"
)

// Notifier is responsible for sending alert notifications as webex messages.
type Notifier struct {
	*receivers.Base
	ns       receivers.WebhookSender
	log      logging.Logger
	images   images.ImageStore
	tmpl     *template.Template
	orgID    int64
	settings Config
}

func New(cfg Config, orgID int64, info receivers.NotifierInfo, template *template.Template, sender receivers.WebhookSender, images images.ImageStore, logger logging.Logger) *Notifier {
	return &Notifier{
		Base:     receivers.NewBase(info),
		orgID:    orgID,
		log:      logger,
		ns:       sender,
		images:   images,
		tmpl:     template,
		settings: cfg,
	}
}

// webexMessage defines the JSON object to send to Webex endpoints.
type webexMessage struct {
	RoomID  string   `json:"roomId,omitempty"`
	Message string   `json:"markdown"`
	Files   []string `json:"files,omitempty"`
}

// Notify implements the Notifier interface.
func (wn *Notifier) Notify(ctx context.Context, as ...*types.Alert) (bool, error) {
	var tmplErr error
	tmpl, data := template2.TmplText(ctx, wn.tmpl, as, wn.log, &tmplErr)

	message, truncated := receivers.TruncateInBytes(tmpl(wn.settings.Message), 4096)
	if truncated {
		wn.log.Warn("Webex message too long, truncating message", "OriginalMessage", wn.settings.Message)
	}

	if tmplErr != nil {
		wn.log.Warn("Failed to template webex message", "Error", tmplErr.Error())
		tmplErr = nil
	}

	msg := &webexMessage{
		RoomID:  wn.settings.RoomID,
		Message: message,
		Files:   []string{},
	}

	// Augment our Alert data with ImageURLs if available.
	_ = images.WithStoredImages(ctx, wn.log, wn.images, func(index int, image images.Image) error {
		// Cisco Webex only supports a single image per request: https://developer.webex.com/docs/basics#message-attachments
		if image.HasURL() {
			data.Alerts[index].ImageURL = image.URL
			msg.Files = append(msg.Files, image.URL)
			return images.ErrImagesDone
		}

		return nil
	}, as...)

	body, err := json.Marshal(msg)
	if err != nil {
		return false, err
	}

	parsedURL := tmpl(wn.settings.APIURL)
	if tmplErr != nil {
		return false, tmplErr
	}

	cmd := &receivers.SendWebhookSettings{
		URL:        parsedURL,
		Body:       string(body),
		HTTPMethod: http.MethodPost,
	}

	if wn.settings.Token != "" {
		headers := make(map[string]string)
		headers["Authorization"] = fmt.Sprintf("Bearer %s", wn.settings.Token)
		cmd.HTTPHeader = headers
	}

	if err := wn.ns.SendWebhook(ctx, cmd); err != nil {
		return false, err
	}

	return true, nil
}

func (wn *Notifier) SendResolved() bool {
	return !wn.GetDisableResolveMessage()
}