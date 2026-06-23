package j16

import (
	"encoding/json"
	"time"
)

// IdTagInfo is the canonical response payload for any idTag interaction.
type IdTagInfo struct {
	Status      AuthorizationStatus `json:"status"`
	ExpiryDate  *time.Time          `json:"expiryDate,omitempty"`
	ParentIdTag string              `json:"parentIdTag,omitempty"`
}

// ----- BootNotification --------------------------------------------------

type BootNotificationReq struct {
	ChargePointVendor       string `json:"chargePointVendor"`
	ChargePointModel        string `json:"chargePointModel"`
	ChargePointSerialNumber string `json:"chargePointSerialNumber,omitempty"`
	ChargeBoxSerialNumber   string `json:"chargeBoxSerialNumber,omitempty"`
	FirmwareVersion         string `json:"firmwareVersion,omitempty"`
	Iccid                   string `json:"iccid,omitempty"`
	Imsi                    string `json:"imsi,omitempty"`
	MeterType               string `json:"meterType,omitempty"`
	MeterSerialNumber       string `json:"meterSerialNumber,omitempty"`
}

type BootNotificationResp struct {
	CurrentTime time.Time          `json:"currentTime"`
	Interval    int                `json:"interval"`
	Status      RegistrationStatus `json:"status"`
}

// ----- Heartbeat ---------------------------------------------------------

type HeartbeatReq struct{}

type HeartbeatResp struct {
	CurrentTime time.Time `json:"currentTime"`
}

// ----- StatusNotification ------------------------------------------------

type StatusNotificationReq struct {
	ConnectorId     int                  `json:"connectorId"`
	ErrorCode       ChargePointErrorCode `json:"errorCode"`
	Status          ChargePointStatus    `json:"status"`
	Info            string               `json:"info,omitempty"`
	Timestamp       *time.Time           `json:"timestamp,omitempty"`
	VendorId        string               `json:"vendorId,omitempty"`
	VendorErrorCode string               `json:"vendorErrorCode,omitempty"`
}

type StatusNotificationResp struct{}

// ----- Authorize ---------------------------------------------------------

type AuthorizeReq struct {
	IdTag string `json:"idTag"`
}

type AuthorizeResp struct {
	IdTagInfo IdTagInfo `json:"idTagInfo"`
}

// ----- StartTransaction --------------------------------------------------

type StartTransactionReq struct {
	ConnectorId   int       `json:"connectorId"`
	IdTag         string    `json:"idTag"`
	MeterStart    int       `json:"meterStart"`
	ReservationId *int      `json:"reservationId,omitempty"`
	Timestamp     time.Time `json:"timestamp"`
}

type StartTransactionResp struct {
	IdTagInfo     IdTagInfo `json:"idTagInfo"`
	TransactionId int       `json:"transactionId"`
}

// ----- StopTransaction ---------------------------------------------------

type StopTransactionReq struct {
	IdTag           string       `json:"idTag,omitempty"`
	MeterStop       int          `json:"meterStop"`
	Timestamp       time.Time    `json:"timestamp"`
	TransactionId   int          `json:"transactionId"`
	Reason          StopReason   `json:"reason,omitempty"`
	TransactionData []MeterValue `json:"transactionData,omitempty"`
}

type StopTransactionResp struct {
	IdTagInfo *IdTagInfo `json:"idTagInfo,omitempty"`
}

// ----- MeterValues -------------------------------------------------------

type MeterValuesReq struct {
	ConnectorId   int          `json:"connectorId"`
	TransactionId *int         `json:"transactionId,omitempty"`
	MeterValue    []MeterValue `json:"meterValue"`
}

type MeterValuesResp struct{}

type MeterValue struct {
	Timestamp    time.Time      `json:"timestamp"`
	SampledValue []SampledValue `json:"sampledValue"`
}

type SampledValue struct {
	Value     string         `json:"value"`
	Context   ReadingContext `json:"context,omitempty"`
	Format    ValueFormat    `json:"format,omitempty"`
	Measurand Measurand      `json:"measurand,omitempty"`
	Phase     Phase          `json:"phase,omitempty"`
	Location  Location       `json:"location,omitempty"`
	Unit      UnitOfMeasure  `json:"unit,omitempty"`
}

// ----- DataTransfer ------------------------------------------------------

type DataTransferReq struct {
	VendorId  string          `json:"vendorId"`
	MessageId string          `json:"messageId,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
}

type DataTransferResp struct {
	Status string          `json:"status"` // Accepted | Rejected | UnknownMessageId | UnknownVendorId
	Data   json.RawMessage `json:"data,omitempty"`
}

// ----- CMS-initiated calls (we receive these) ----------------------------

type RemoteStartTransactionReq struct {
	ConnectorId     *int            `json:"connectorId,omitempty"`
	IdTag           string          `json:"idTag"`
	ChargingProfile json.RawMessage `json:"chargingProfile,omitempty"`
}

type RemoteStartTransactionResp struct {
	Status string `json:"status"` // Accepted | Rejected
}

type RemoteStopTransactionReq struct {
	TransactionId int `json:"transactionId"`
}

type RemoteStopTransactionResp struct {
	Status string `json:"status"`
}

type ResetReq struct {
	Type string `json:"type"` // Hard | Soft
}

type ResetResp struct {
	Status string `json:"status"`
}

type ChangeAvailabilityReq struct {
	ConnectorId int    `json:"connectorId"`
	Type        string `json:"type"` // Inoperative | Operative
}

type ChangeAvailabilityResp struct {
	Status string `json:"status"` // Accepted | Rejected | Scheduled
}

type TriggerMessageReq struct {
	RequestedMessage string `json:"requestedMessage"`
	ConnectorId      *int   `json:"connectorId,omitempty"`
}

type TriggerMessageResp struct {
	Status string `json:"status"` // Accepted | Rejected | NotImplemented
}

type UnlockConnectorReq struct {
	ConnectorId int `json:"connectorId"`
}

type UnlockConnectorResp struct {
	Status string `json:"status"` // Unlocked | UnlockFailed | NotSupported
}
