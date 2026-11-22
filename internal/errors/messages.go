package errors

const (
	TypeError                   = "error"
	TypeAuthNotFound            = "auth_not_found"
	TypeModelNotFound           = "model_not_found"
	TypeRequestRetryExceeded    = "request_retry_exceeded"
	TypeUserCreditsInsufficient = "user_credits_insufficient"
	TypeUserPeriodLimitExceeded = "user_period_limit_exceeded"
)

const (
	MessageError                            = "error"
	MessageRuntimeNotReady                  = "runtime not ready"
	MessageWrongNumberOfArgumentsGet        = "wrong number of arguments for 'get' command"
	MessageWrongNumberOfArgumentsRPOP       = "wrong number of arguments for 'rpop' command"
	MessageInvalidRequestJSON               = "invalid request json"
	MessageUnsupportedType                  = "unsupported type"
	MessageMissingAuthIndex                 = "missing auth_index"
	MessageAuthNotFound                     = "auth not found"
	MessageNoDispatchResult                 = "no dispatch result"
	MessageNoAuthAvailable                  = "no auth available"
	MessageMissingModel                     = "missing model"
	MessageMissingRequiredCredentialHeaders = "missing required credential headers"
	MessageInvalidAPIKey                    = "invalid api key"
	MessageRequestRetryExceeded             = "request retry limit exceeded"
	MessageUserCreditsInsufficient          = "insufficient user credits"
	MessageUserPeriodLimitExceeded          = "period limit exceeded"
	// Member spend limits reuse TypeUserPeriodLimitExceeded so CPA/AI48Proxy
	// nodes map them to 429 without a payload contract change.
	MessageOrganizationMemberLimitExceeded = "organization member spend limit exceeded"
	MessageOrganizationMembershipRevoked   = "organization membership revoked"

	MessageModelDoesNotExistFmt       = "model %s does not exist"
	MessageModelPriceNotConfiguredFmt = "model %s has no configured price"
)
