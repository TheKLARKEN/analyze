package handlers

import (
	"context"
	"net/http"
	"time"

	"github.com/go-openapi/runtime/middleware"
	"github.com/sirupsen/logrus"

	"github.com/supergiant/robot/pkg/api/operations"
	"github.com/supergiant/robot/pkg/models"
	"github.com/supergiant/robot/pkg/storage"
)

type checkResultsHandler struct {
	storage storage.Interface
	log     logrus.FieldLogger
}

func NewCheckResultsHandler(storage storage.Interface, logger logrus.FieldLogger) operations.GetCheckResultsHandler {
	return &checkResultsHandler{
		storage: storage,
		log:     logger,
	}
}

func (h *checkResultsHandler) Handle(params operations.GetCheckResultsParams) middleware.Responder {
	h.log.Debugf("got request at: %v, request: %+v", time.Now(), params)
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	resultsRaw, err := h.storage.GetAll(ctx, models.CheckResultPrefix)

	if err != nil {
		r := operations.NewGetCheckResultsDefault(http.StatusInternalServerError)
		msg := err.Error()
		r.Payload = &models.Error{
			Code:    http.StatusInternalServerError,
			Message: &msg,
		}
		return r
	}

	result := &operations.GetCheckResultsOKBody{
		CheckResults: []*models.CheckResult{},
	}

	for _, rawResult := range resultsRaw {
		checkResult := &models.CheckResult{}
		err := checkResult.UnmarshalBinary(rawResult)
		if err != nil {
			r := operations.NewGetCheckResultsDefault(http.StatusInternalServerError)
			msg := err.Error()
			r.Payload = &models.Error{
				Code:    http.StatusInternalServerError,
				Message: &msg,
			}
			return r
		}
		result.CheckResults = append(result.CheckResults, checkResult)
	}
	h.log.Debugf("request processing finished at: %v, request: %+v", time.Now(), params)

	return operations.NewGetCheckResultsOK().WithPayload(result)
}