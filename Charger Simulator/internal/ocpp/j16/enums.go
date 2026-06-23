package j16

// ChargePointStatus is the connector-level status reported via StatusNotification.
type ChargePointStatus string

const (
	StatusAvailable     ChargePointStatus = "Available"
	StatusPreparing     ChargePointStatus = "Preparing"
	StatusCharging      ChargePointStatus = "Charging"
	StatusSuspendedEV   ChargePointStatus = "SuspendedEV"
	StatusSuspendedEVSE ChargePointStatus = "SuspendedEVSE"
	StatusFinishing     ChargePointStatus = "Finishing"
	StatusReserved      ChargePointStatus = "Reserved"
	StatusUnavailable   ChargePointStatus = "Unavailable"
	StatusFaulted       ChargePointStatus = "Faulted"
)

// ChargePointErrorCode accompanies every StatusNotification (NoError when healthy).
type ChargePointErrorCode string

const (
	ErrorNoError              ChargePointErrorCode = "NoError"
	ErrorConnectorLockFailure ChargePointErrorCode = "ConnectorLockFailure"
	ErrorEVCommunicationError ChargePointErrorCode = "EVCommunicationError"
	ErrorGroundFailure        ChargePointErrorCode = "GroundFailure"
	ErrorHighTemperature      ChargePointErrorCode = "HighTemperature"
	ErrorInternalError        ChargePointErrorCode = "InternalError"
	ErrorLocalListConflict    ChargePointErrorCode = "LocalListConflict"
	ErrorOtherError           ChargePointErrorCode = "OtherError"
	ErrorOverCurrentFailure   ChargePointErrorCode = "OverCurrentFailure"
	ErrorOverVoltage          ChargePointErrorCode = "OverVoltage"
	ErrorPowerMeterFailure    ChargePointErrorCode = "PowerMeterFailure"
	ErrorPowerSwitchFailure   ChargePointErrorCode = "PowerSwitchFailure"
	ErrorReaderFailure        ChargePointErrorCode = "ReaderFailure"
	ErrorResetFailure         ChargePointErrorCode = "ResetFailure"
	ErrorUnderVoltage         ChargePointErrorCode = "UnderVoltage"
	ErrorWeakSignal           ChargePointErrorCode = "WeakSignal"
)

// AuthorizationStatus is the result of Authorize / Start / Stop transaction.
type AuthorizationStatus string

const (
	AuthAccepted     AuthorizationStatus = "Accepted"
	AuthBlocked      AuthorizationStatus = "Blocked"
	AuthExpired      AuthorizationStatus = "Expired"
	AuthInvalid      AuthorizationStatus = "Invalid"
	AuthConcurrentTx AuthorizationStatus = "ConcurrentTx"
)

// RegistrationStatus is the result of BootNotification.
type RegistrationStatus string

const (
	RegAccepted RegistrationStatus = "Accepted"
	RegPending  RegistrationStatus = "Pending"
	RegRejected RegistrationStatus = "Rejected"
)

// StopReason explains why a transaction stopped.
type StopReason string

const (
	StopReasonEmergencyStop  StopReason = "EmergencyStop"
	StopReasonEVDisconnected StopReason = "EVDisconnected"
	StopReasonHardReset      StopReason = "HardReset"
	StopReasonLocal          StopReason = "Local"
	StopReasonOther          StopReason = "Other"
	StopReasonPowerLoss      StopReason = "PowerLoss"
	StopReasonReboot         StopReason = "Reboot"
	StopReasonRemote         StopReason = "Remote"
	StopReasonSoftReset      StopReason = "SoftReset"
	StopReasonUnlockCommand  StopReason = "UnlockCommand"
	StopReasonDeAuthorized   StopReason = "DeAuthorized"
)

// Measurand identifies what a SampledValue represents.
type Measurand string

const (
	MeasurandEnergyActiveImportRegister Measurand = "Energy.Active.Import.Register"
	MeasurandPowerActiveImport          Measurand = "Power.Active.Import"
	MeasurandVoltage                    Measurand = "Voltage"
	MeasurandCurrentImport              Measurand = "Current.Import"
	MeasurandSoC                        Measurand = "SoC"
	MeasurandTemperature                Measurand = "Temperature"
	MeasurandFrequency                  Measurand = "Frequency"
	MeasurandPowerFactor                Measurand = "Power.Factor"
)

// ReadingContext explains the circumstance under which a sample was taken.
type ReadingContext string

const (
	ReadingContextSamplePeriodic   ReadingContext = "Sample.Periodic"
	ReadingContextSampleClock      ReadingContext = "Sample.Clock"
	ReadingContextTransactionBegin ReadingContext = "Transaction.Begin"
	ReadingContextTransactionEnd   ReadingContext = "Transaction.End"
	ReadingContextInterruptionBegin ReadingContext = "Interruption.Begin"
	ReadingContextInterruptionEnd   ReadingContext = "Interruption.End"
)

// ValueFormat is "Raw" or "SignedData".
type ValueFormat string

const (
	FormatRaw        ValueFormat = "Raw"
	FormatSignedData ValueFormat = "SignedData"
)

// Location is where in the charger a sample was taken.
type Location string

const (
	LocationBody   Location = "Body"
	LocationCable  Location = "Cable"
	LocationEV     Location = "EV"
	LocationInlet  Location = "Inlet"
	LocationOutlet Location = "Outlet"
)

// UnitOfMeasure for SampledValue.
type UnitOfMeasure string

const (
	UnitWh      UnitOfMeasure = "Wh"
	UnitKWh     UnitOfMeasure = "kWh"
	UnitW       UnitOfMeasure = "W"
	UnitKW      UnitOfMeasure = "kW"
	UnitV       UnitOfMeasure = "V"
	UnitA       UnitOfMeasure = "A"
	UnitCelsius UnitOfMeasure = "Celsius"
	UnitPercent UnitOfMeasure = "Percent"
	UnitHertz   UnitOfMeasure = "Hertz"
)

// Phase identifies which electrical phase a SampledValue refers to.
type Phase string

const (
	PhaseL1  Phase = "L1"
	PhaseL2  Phase = "L2"
	PhaseL3  Phase = "L3"
	PhaseN   Phase = "N"
	PhaseL1N Phase = "L1-N"
	PhaseL2N Phase = "L2-N"
	PhaseL3N Phase = "L3-N"
)
