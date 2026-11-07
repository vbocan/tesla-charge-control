package deye

// Deye SUN-6K-SG03LP1-EU Modbus holding register addresses.
// Source: https://github.com/kbialek/deye-inverter-mqtt (metric group deye_sg03lp1)

const (
	// Solar / DC
	RegPV1Voltage       = 109
	RegPV1Current       = 110
	RegPV2Voltage       = 111
	RegPV2Current       = 112
	RegPV1Power         = 186
	RegPV2Power         = 187
	RegDailyProduction  = 108
	RegTotalProductionL = 96 // low word
	RegTotalProductionH = 97 // high word

	// Grid
	RegGridVoltageL1  = 150
	RegGridFrequency  = 79
	RegGridPowerTotal = 169 // signed: + importing, - exporting
	RegInternalCTL1   = 167
	RegExternalCTL1   = 170
	RegDailyBought    = 76
	RegDailySold      = 77
	RegTotalSoldL     = 81 // low word
	RegTotalSoldH     = 82 // high word

	// Inverter AC output
	RegInvVoltageL1  = 154
	RegInvFrequency  = 192
	RegInvPowerL1    = 173 // signed
	RegInvPowerTotal = 175 // signed
	RegInvCurrentL1  = 164 // signed

	// Battery
	RegDailyBattCharge     = 70
	RegDailyBattDischarge  = 71
	RegTotalBattChargeL    = 72  // low word
	RegTotalBattChargeH    = 73  // high word
	RegTotalBattDischargeL = 74  // low word
	RegTotalBattDischargeH = 75  // high word
	RegBattTemp            = 182 // scale 0.1, offset -100
	RegBattVoltage         = 183
	RegBattSOC             = 184
	RegBattPower           = 190 // signed: + discharging, - charging
	RegBattCurrent         = 191 // signed

	// Load
	RegLoadPowerTotal = 178 // direct read, total load
	RegLoadPowerL1    = 176

	// Temperature
	RegDCTemp = 90 // signed, scale 0.1, offset -100
	RegACTemp = 91 // signed, scale 0.1, offset -100
)
