package iot

import (
	"github.com/scaleway/scaleway-cli/internal/core"
	"github.com/scaleway/scaleway-cli/internal/human"
	iot "github.com/scaleway/scaleway-sdk-go/api/iot/v1"
)

func GetCommands() *core.Commands {
	cmds := GetGeneratedCommands()

	human.RegisterMarshalerFunc(iot.HubStatus(""), human.EnumMarshalFunc(hubStatusMarshalSpecs))
	human.RegisterMarshalerFunc(iot.DeviceMessageFiltersRulePolicy(""), human.EnumMarshalFunc(deviceMessageFiltersPolicyMarshalSpecs))
	human.RegisterMarshalerFunc(iot.DeviceStatus(""), human.EnumMarshalFunc(deviceStatusMarshalSpecs))

	return cmds
}
