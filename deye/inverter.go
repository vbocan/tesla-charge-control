package deye

// InverterData holds parsed register values from the Deye inverter.
type InverterData struct {
	// PV (DC)
	PV1V    float64 `json:"pv1_v"`
	PV1A    float64 `json:"pv1_a"`
	PV1W    int     `json:"pv1_w"`
	PV2V    float64 `json:"pv2_v"`
	PV2A    float64 `json:"pv2_a"`
	PV2W    int     `json:"pv2_w"`
	TotalPV int     `json:"total_pv"`
	// Grid
	GridVoltage float64 `json:"grid_voltage"`
	GridFreq    float64 `json:"grid_freq"`
	GridPower   int     `json:"grid_power"`
	CTL1        int     `json:"ct_l1"`
	ExtCTL1     int     `json:"ext_ct_l1"`
	// Inverter AC output
	InvVoltage float64 `json:"inv_voltage"`
	InvFreq    float64 `json:"inv_freq"`
	InvL1Power int     `json:"inv_l1_power"`
	InvTotal   int     `json:"inv_total"`
	InvCurrent float64 `json:"inv_current"`
	// Battery
	BattSOC            int     `json:"batt_soc"`
	BattVoltage        float64 `json:"batt_voltage"`
	BattCurrent        float64 `json:"batt_current"`
	BattPower          int     `json:"batt_power"`
	BattTemp           float64 `json:"batt_temp"`
	DailyBattCharge    float64 `json:"daily_batt_charge"`
	DailyBattDischarge float64 `json:"daily_batt_discharge"`
	TotalBattCharge    float64 `json:"total_batt_charge"`
	TotalBattDischarge float64 `json:"total_batt_discharge"`
	// Load
	LoadPower       int `json:"load_power"`
	SelfConsumption int `json:"self_consumption"`
	// Energy counters
	DailyProduction  float64 `json:"daily_production"`
	TotalProduction  float64 `json:"total_production"`
	DailyBought      float64 `json:"daily_bought"`
	DailySold        float64 `json:"daily_sold"`
	DailyConsumption float64 `json:"daily_consumption"`
	TotalSold        float64 `json:"total_sold"`
	// Temperature
	DCTemp float64 `json:"dc_temp"`
	ACTemp float64 `json:"ac_temp"`
}

// selfConsumption computes the inverter's own parasitic draw from the energy balance:
//
//	self = PV + battDischarge + gridImport - battCharge - gridExport - load
//
// battPower: + discharging, - charging.  gridPower: + importing, - exporting.
func selfConsumption(pvW, battPower, gridPower, loadPower int) int {
	// Sources: PV generation + battery discharge (when positive) + grid import (when positive)
	// Sinks: battery charge (when negative) + grid export (when negative) + load
	sc := pvW + battPower + gridPower - loadPower
	if sc < 0 {
		sc = 0
	}
	return sc
}

// toSigned16 converts an unsigned 16-bit value to signed (two's complement).
func toSigned16(v uint16) int {
	if v > 0x7FFF {
		return int(v) - 0x10000
	}
	return int(v)
}

// doubleReg combines two 16-bit registers into a 32-bit unsigned value.
func doubleReg(regs map[int]uint16, low, high int) uint32 {
	return uint32(regs[low]) + (uint32(regs[high]) << 16)
}

// ParseRegisters converts raw register values into structured inverter data.
func ParseRegisters(regs map[int]uint16) InverterData {
	pv1W := int(regs[RegPV1Power])
	pv2W := int(regs[RegPV2Power])

	gridPower := toSigned16(regs[RegGridPowerTotal])
	invL1Power := toSigned16(regs[RegInvPowerL1])
	invTotal := toSigned16(regs[RegInvPowerTotal])
	loadPower := toSigned16(regs[RegLoadPowerTotal])
	battPower := toSigned16(regs[RegBattPower])

	return InverterData{
		PV1V:    float64(regs[RegPV1Voltage]) * 0.1,
		PV1A:    float64(regs[RegPV1Current]) * 0.1,
		PV1W:    pv1W,
		PV2V:    float64(regs[RegPV2Voltage]) * 0.1,
		PV2A:    float64(regs[RegPV2Current]) * 0.1,
		PV2W:    pv2W,
		TotalPV: pv1W + pv2W,

		GridVoltage: float64(regs[RegGridVoltageL1]) * 0.1,
		GridFreq:    float64(regs[RegGridFrequency]) * 0.01,
		GridPower:   gridPower,
		CTL1:        toSigned16(regs[RegInternalCTL1]),
		ExtCTL1:     toSigned16(regs[RegExternalCTL1]),

		InvVoltage: float64(regs[RegInvVoltageL1]) * 0.1,
		InvFreq:    float64(regs[RegInvFrequency]) * 0.01,
		InvL1Power: invL1Power,
		InvTotal:   invTotal,
		InvCurrent: float64(toSigned16(regs[RegInvCurrentL1])) * 0.01,

		BattSOC:            int(regs[RegBattSOC]),
		BattVoltage:        float64(regs[RegBattVoltage]) * 0.01,
		BattCurrent:        float64(toSigned16(regs[RegBattCurrent])) * 0.01,
		BattPower:          battPower,
		BattTemp:           float64(regs[RegBattTemp])*0.1 - 100,
		DailyBattCharge:    float64(regs[RegDailyBattCharge]) * 0.1,
		DailyBattDischarge: float64(regs[RegDailyBattDischarge]) * 0.1,
		TotalBattCharge:    float64(doubleReg(regs, RegTotalBattChargeL, RegTotalBattChargeH)) * 0.1,
		TotalBattDischarge: float64(doubleReg(regs, RegTotalBattDischargeL, RegTotalBattDischargeH)) * 0.1,

		LoadPower:       loadPower,
		SelfConsumption: selfConsumption(pv1W+pv2W, battPower, gridPower, loadPower),

		DailyProduction:  float64(regs[RegDailyProduction]) * 0.1,
		TotalProduction:  float64(doubleReg(regs, RegTotalProductionL, RegTotalProductionH)) * 0.1,
		DailyBought:      float64(regs[RegDailyBought]) * 0.1,
		DailySold:        float64(regs[RegDailySold]) * 0.1,
		DailyConsumption: float64(int(regs[RegDailyProduction])+int(regs[RegDailyBought])-int(regs[RegDailySold])-int(regs[RegDailyBattCharge])+int(regs[RegDailyBattDischarge])) * 0.1,
		TotalSold:        float64(doubleReg(regs, RegTotalSoldL, RegTotalSoldH)) * 0.1,

		DCTemp: float64(toSigned16(regs[RegDCTemp]))*0.1 - 100,
		ACTemp: float64(toSigned16(regs[RegACTemp]))*0.1 - 100,
	}
}
