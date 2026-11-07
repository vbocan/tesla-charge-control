package deye

import (
	"fmt"
	"math"
	"time"
)

// SunTimes returns (sunrise, sunset) as fractional hours in local time for today.
// Uses the NOAA simplified solar position algorithm.
func SunTimes(lat, lon float64) (float64, float64) {
	now := time.Now().UTC()
	_, offset := time.Now().Zone()
	utcOffset := float64(offset) / 3600.0

	yday := now.YearDay()
	gamma := 2 * math.Pi / 365 * float64(yday-1)

	// Equation of time (minutes)
	eqtime := 229.18 * (0.000075 +
		0.001868*math.Cos(gamma) -
		0.032077*math.Sin(gamma) -
		0.014615*math.Cos(2*gamma) -
		0.040849*math.Sin(2*gamma))

	// Solar declination (radians)
	decl := 0.006918 -
		0.399912*math.Cos(gamma) +
		0.070257*math.Sin(gamma) -
		0.006758*math.Cos(2*gamma) +
		0.000907*math.Sin(2*gamma) -
		0.002697*math.Cos(3*gamma) +
		0.00148*math.Sin(3*gamma)

	// Hour angle
	latRad := lat * math.Pi / 180
	cosHA := math.Cos(90.833*math.Pi/180)/(math.Cos(latRad)*math.Cos(decl)) -
		math.Tan(latRad)*math.Tan(decl)
	cosHA = math.Max(-1.0, math.Min(1.0, cosHA))
	ha := math.Acos(cosHA) * 180 / math.Pi

	sunriseUTC := 720 - 4*(lon+ha) - eqtime
	sunsetUTC := 720 - 4*(lon-ha) - eqtime

	return sunriseUTC/60 + utcOffset, sunsetUTC/60 + utcOffset
}

// FmtHM formats fractional hours as "Xh YYm".
func FmtHM(hours float64) string {
	h := int(hours)
	m := int(math.Mod(hours, 1) * 60)
	return fmt.Sprintf("%dh %02dm", h, m)
}
