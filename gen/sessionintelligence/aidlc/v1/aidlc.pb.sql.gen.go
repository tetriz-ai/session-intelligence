// Copyright 2026 Workloom Solutions Private Limited
// SPDX-License-Identifier: MIT

// SQL Valuer/Scanner implementations for the aidlc types stored in the database:
// the enum-as-text columns and the JSONB verdict wrappers. This file is maintained
// by hand; keep it in sync with aidlc.proto when a stored type changes.

package coreaidlcv1

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"

	"google.golang.org/protobuf/encoding/protojson"
)

// Valuer interface implementation for storing the AidlcStage in string format in DB
func (p AidlcStage) Value() (driver.Value, error) {
	return p.String(), nil
}

// Scanner interface implementation for parsing AidlcStage while reading from DB
func (p *AidlcStage) Scan(value interface{}) error {
	val, ok := value.(string)
	if !ok {
		return fmt.Errorf("type conversion to string failed, src: %T", val)
	}

	valInt, ok := AidlcStage_value[val]
	if !ok {
		return fmt.Errorf("unexpected AidlcStage value: %s", val)
	}
	*p = AidlcStage(valInt)
	return nil
}

// Marshaler interface implementation for AidlcStage
func (x AidlcStage) MarshalJSON() ([]byte, error) {
	return json.Marshal(x.String())
}

// Unmarshaler interface implementation for AidlcStage
func (x *AidlcStage) UnmarshalJSON(b []byte) error {
	var val string
	err := json.Unmarshal(b, &val)
	if err != nil {
		return err
	}
	*x = AidlcStage(AidlcStage_value[val])
	return nil
}

// Valuer interface implementation for storing the HarnessClassificationTask in string format in DB
func (p HarnessClassificationTask) Value() (driver.Value, error) {
	return p.String(), nil
}

// Scanner interface implementation for parsing HarnessClassificationTask while reading from DB
func (p *HarnessClassificationTask) Scan(value interface{}) error {
	val, ok := value.(string)
	if !ok {
		return fmt.Errorf("type conversion to string failed, src: %T", val)
	}

	valInt, ok := HarnessClassificationTask_value[val]
	if !ok {
		return fmt.Errorf("unexpected HarnessClassificationTask value: %s", val)
	}
	*p = HarnessClassificationTask(valInt)
	return nil
}

// Marshaler interface implementation for HarnessClassificationTask
func (x HarnessClassificationTask) MarshalJSON() ([]byte, error) {
	return json.Marshal(x.String())
}

// Unmarshaler interface implementation for HarnessClassificationTask
func (x *HarnessClassificationTask) UnmarshalJSON(b []byte) error {
	var val string
	err := json.Unmarshal(b, &val)
	if err != nil {
		return err
	}
	*x = HarnessClassificationTask(HarnessClassificationTask_value[val])
	return nil
}

// Valuer interface implementation for storing the HarnessClassificationAttemptState in string format in DB
func (p HarnessClassificationAttemptState) Value() (driver.Value, error) {
	return p.String(), nil
}

// Scanner interface implementation for parsing HarnessClassificationAttemptState while reading from DB
func (p *HarnessClassificationAttemptState) Scan(value interface{}) error {
	val, ok := value.(string)
	if !ok {
		return fmt.Errorf("type conversion to string failed, src: %T", val)
	}

	valInt, ok := HarnessClassificationAttemptState_value[val]
	if !ok {
		return fmt.Errorf("unexpected HarnessClassificationAttemptState value: %s", val)
	}
	*p = HarnessClassificationAttemptState(valInt)
	return nil
}

// Marshaler interface implementation for HarnessClassificationAttemptState
func (x HarnessClassificationAttemptState) MarshalJSON() ([]byte, error) {
	return json.Marshal(x.String())
}

// Unmarshaler interface implementation for HarnessClassificationAttemptState
func (x *HarnessClassificationAttemptState) UnmarshalJSON(b []byte) error {
	var val string
	err := json.Unmarshal(b, &val)
	if err != nil {
		return err
	}
	*x = HarnessClassificationAttemptState(HarnessClassificationAttemptState_value[val])
	return nil
}

// Valuer interface implementation for storing the ArtifactIntentClass in string format in DB
func (p ArtifactIntentClass) Value() (driver.Value, error) {
	return p.String(), nil
}

// Scanner interface implementation for parsing ArtifactIntentClass while reading from DB
func (p *ArtifactIntentClass) Scan(value interface{}) error {
	val, ok := value.(string)
	if !ok {
		return fmt.Errorf("type conversion to string failed, src: %T", val)
	}

	valInt, ok := ArtifactIntentClass_value[val]
	if !ok {
		return fmt.Errorf("unexpected ArtifactIntentClass value: %s", val)
	}
	*p = ArtifactIntentClass(valInt)
	return nil
}

// Marshaler interface implementation for ArtifactIntentClass
func (x ArtifactIntentClass) MarshalJSON() ([]byte, error) {
	return json.Marshal(x.String())
}

// Unmarshaler interface implementation for ArtifactIntentClass
func (x *ArtifactIntentClass) UnmarshalJSON(b []byte) error {
	var val string
	err := json.Unmarshal(b, &val)
	if err != nil {
		return err
	}
	*x = ArtifactIntentClass(ArtifactIntentClass_value[val])
	return nil
}

// Valuer interface implementation for storing the IntentClassSource in string format in DB
func (p IntentClassSource) Value() (driver.Value, error) {
	return p.String(), nil
}

// Scanner interface implementation for parsing IntentClassSource while reading from DB
func (p *IntentClassSource) Scan(value interface{}) error {
	val, ok := value.(string)
	if !ok {
		return fmt.Errorf("type conversion to string failed, src: %T", val)
	}

	valInt, ok := IntentClassSource_value[val]
	if !ok {
		return fmt.Errorf("unexpected IntentClassSource value: %s", val)
	}
	*p = IntentClassSource(valInt)
	return nil
}

// Marshaler interface implementation for IntentClassSource
func (x IntentClassSource) MarshalJSON() ([]byte, error) {
	return json.Marshal(x.String())
}

// Unmarshaler interface implementation for IntentClassSource
func (x *IntentClassSource) UnmarshalJSON(b []byte) error {
	var val string
	err := json.Unmarshal(b, &val)
	if err != nil {
		return err
	}
	*x = IntentClassSource(IntentClassSource_value[val])
	return nil
}

// Scanner interface implementation for parsing HarnessClassificationVerdict while reading from DB
func (a *HarnessClassificationVerdict) Scan(src interface{}) error {
	marshalledData, ok := src.([]byte)
	if !ok {
		return fmt.Errorf("type assertion to []byte failed, src: %T", src)
	}
	unmarshalOptions := &protojson.UnmarshalOptions{}
	unmarshalOptions.DiscardUnknown = true
	return unmarshalOptions.Unmarshal(marshalledData, a)
}

// Valuer interface implementation for storing the HarnessClassificationVerdict in string format in DB
func (a *HarnessClassificationVerdict) Value() (driver.Value, error) {
	if a == nil {
		return nil, nil
	}
	return protojson.Marshal(a)
}

// Marshaler interface implementation for HarnessClassificationVerdict
func (a *HarnessClassificationVerdict) MarshalJSON() ([]byte, error) {
	return protojson.Marshal(a)
}

// Unmarshaler interface implementation for HarnessClassificationVerdict
func (a *HarnessClassificationVerdict) UnmarshalJSON(b []byte) error {
	return protojson.Unmarshal(b, a)
}

// Scanner interface implementation for parsing HarnessClassificationVerdicts while reading from DB
func (a *HarnessClassificationVerdicts) Scan(src interface{}) error {
	marshalledData, ok := src.([]byte)
	if !ok {
		return fmt.Errorf("type assertion to []byte failed, src: %T", src)
	}
	unmarshalOptions := &protojson.UnmarshalOptions{}
	unmarshalOptions.DiscardUnknown = true
	return unmarshalOptions.Unmarshal(marshalledData, a)
}

// Valuer interface implementation for storing the HarnessClassificationVerdicts in string format in DB
func (a *HarnessClassificationVerdicts) Value() (driver.Value, error) {
	if a == nil {
		return nil, nil
	}
	return protojson.Marshal(a)
}

// Marshaler interface implementation for HarnessClassificationVerdicts
func (a *HarnessClassificationVerdicts) MarshalJSON() ([]byte, error) {
	return protojson.Marshal(a)
}

// Unmarshaler interface implementation for HarnessClassificationVerdicts
func (a *HarnessClassificationVerdicts) UnmarshalJSON(b []byte) error {
	return protojson.Unmarshal(b, a)
}
