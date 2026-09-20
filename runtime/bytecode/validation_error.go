package bytecode

import (
	"errors"
)

type ValidationError struct {
	Code string
	Path string
	Err  error
}

const (
	ValidationArtifactNil                = "ir.artifact.nil"
	ValidationArtifactFormatUnsupported  = "ir.artifact.format.unsupported"
	ValidationArtifactVersionUnsupported = "ir.artifact.version.unsupported"
	ValidationArtifactOpcodeUnsupported  = "ir.artifact.opcode_set.unsupported"
	ValidationRequirementInvalid         = "ir.requirement.invalid"
	ValidationLimitExceeded              = "ir.limit.exceeded"
	ValidationFieldMissing               = "ir.field.missing"
	ValidationReferenceUnknown           = "ir.reference.unknown"
	ValidationOpcodeUnknown              = "ir.opcode.unknown"
	ValidationPayloadUnexpected          = "ir.payload.unexpected"
	ValidationPayloadInvalidJSON         = "ir.payload.invalid_json"
	ValidationIDDuplicate                = "ir.id.duplicate"
	ValidationLabelDuplicate             = "ir.label.duplicate"
	ValidationLabelUnknown               = "ir.label.unknown"
	ValidationDebugFileDuplicate         = "ir.debug.file.duplicate"
	ValidationReferenceOutOfRange        = "ir.reference.out_of_range"
	ValidationSchemaMismatch             = "ir.schema.mismatch"
	ValidationValueUnsupported           = "ir.value.unsupported"
	ValidationTypeMethodDuplicate        = "ir.type.method.duplicate"
	ValidationStackUnderflow             = "ir.stack.underflow"
	ValidationStackUnbalanced            = "ir.stack.unbalanced"
	ValidationFailed                     = "ir.validation.failed"
)

func newValidationError(path string, err error) ValidationError {
	return ValidationError{Code: ValidationFailed, Path: path, Err: err}
}

func newCodedValidationError(code, path string, err error) ValidationError {
	return ValidationError{Code: code, Path: path, Err: err}
}

func missingValidationError(path string, err error) ValidationError {
	return newCodedValidationError(ValidationFieldMissing, path, err)
}

func unknownValidationError(path string, err error) ValidationError {
	return newCodedValidationError(ValidationReferenceUnknown, path, err)
}

func schemaMismatchValidationError(path string, err error) ValidationError {
	return newCodedValidationError(ValidationSchemaMismatch, path, err)
}

func unsupportedValueValidationError(path string, err error) ValidationError {
	return newCodedValidationError(ValidationValueUnsupported, path, err)
}

func outOfRangeValidationError(path string, err error) ValidationError {
	return newCodedValidationError(ValidationReferenceOutOfRange, path, err)
}

func (e ValidationError) Error() string {
	if e.Path == "" {
		return e.Err.Error()
	}
	return e.Path + ": " + e.Err.Error()
}

func (e ValidationError) Unwrap() error {
	return e.Err
}

type ValidationIssue struct {
	Code    string `json:"code"`
	Path    string `json:"path,omitempty"`
	Message string `json:"message"`
}

func (e ValidationError) Issue() ValidationIssue {
	code := e.Code
	if code == "" {
		code = ValidationFailed
	}
	message := ""
	if e.Err != nil {
		message = e.Err.Error()
	}
	return ValidationIssue{Code: code, Path: e.Path, Message: message}
}

func ValidationIssueFromError(err error) (ValidationIssue, bool) {
	if err == nil {
		return ValidationIssue{}, false
	}
	var validationErr ValidationError
	if errors.As(err, &validationErr) {
		return validationErr.Issue(), true
	}
	return ValidationIssue{Code: ValidationFailed, Message: err.Error()}, true
}

func ValidationCode(err error) string {
	issue, ok := ValidationIssueFromError(err)
	if !ok {
		return ""
	}
	return issue.Code
}
