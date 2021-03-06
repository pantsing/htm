package encoders

import (
	"fmt"
	//"github.com/cznic/mathutil"
	//"github.com/zacg/floats"
	//"github.com/nupic-community/htm"
	"github.com/nupic-community/htm/utils"
	//"github.com/zacg/ints"
	//"math"
	"time"
)

/*
	Params for the date encoder
*/
type DateEncoderParams struct {
	HolidayWidth    int
	HolidayRadius   float64
	SeasonWidth     int
	SeasonRadius    float64
	DayOfWeekWidth  int
	DayOfWeekRadius float64
	WeekendWidth    int
	WeekendRadius   float64
	TimeOfDayWidth  int
	TimeOfDayRadius float64
	//CustomDays     int
	Name string
	//list of holidays stored as {mm,dd}
	Holidays []utils.TupleInt
}

func NewDateEncoderParams() *DateEncoderParams {
	p := new(DateEncoderParams)

	//set defaults
	p.SeasonWidth = 3
	p.DayOfWeekWidth = 1
	p.WeekendWidth = 3
	p.TimeOfDayWidth = 5

	p.SeasonRadius = 91.5 //days
	p.DayOfWeekRadius = 1
	p.TimeOfDayRadius = 4
	p.WeekendRadius = 1
	p.HolidayRadius = 1

	p.Holidays = []utils.TupleInt{{12, 25}}

	return p
}

/*
	Date encoder encodes a datetime to a SDR. Params allow for tuning
	for specific date attributes
*/
type DateEncoder struct {
	DateEncoderParams
	seasonEncoder    *ScalerEncoder
	holidayEncoder   *ScalerEncoder
	dayOfWeekEncoder *ScalerEncoder
	weekendEncoder   *ScalerEncoder
	timeOfDayEncoder *ScalerEncoder

	width           int
	seasonOffset    int
	weekendOffset   int
	dayOfWeekOffset int
	holidayOffset   int
	timeOfDayOffset int
}

/*
	Intializes a new date encoder
*/
func NewDateEncoder(params *DateEncoderParams) *DateEncoder {
	de := new(DateEncoder)

	de.DateEncoderParams = *params

	de.width = 0

	if params.SeasonWidth != 0 {
		// Ignore leapyear differences -- assume 366 days in a year
		// Radius = 91.5 days = length of season
		// Value is number of days since beginning of year (0 - 355)

		sep := NewScalerEncoderParams(params.SeasonWidth, 0, 366)
		sep.Name = "Season"
		sep.Periodic = true
		sep.Radius = de.SeasonRadius
		de.seasonEncoder = NewScalerEncoder(sep)
		de.seasonOffset = de.width
		de.width += de.seasonEncoder.N
	}

	if params.DayOfWeekWidth != 0 {
		// Value is day of week (floating point)
		// Radius is 1 day

		sep := NewScalerEncoderParams(params.DayOfWeekWidth, 0, 7)
		sep.Name = "day of week"
		sep.Radius = de.DayOfWeekRadius
		sep.Periodic = true
		de.dayOfWeekEncoder = NewScalerEncoder(sep)
		de.dayOfWeekOffset = de.width
		de.width += de.dayOfWeekEncoder.N
	}

	if params.WeekendWidth != 0 {
		// Binary value. Not sure if this makes sense. Also is somewhat redundant
		// with dayOfWeek
		//Append radius if it was not provided

		sep := NewScalerEncoderParams(params.WeekendWidth, 0, 1)
		sep.Name = "weekend"
		sep.Radius = params.WeekendRadius
		de.weekendEncoder = NewScalerEncoder(sep)
		de.weekendOffset = de.width
		de.width += de.weekendEncoder.N
	}

	if params.HolidayWidth > 0 {
		// A "continuous" binary value. = 1 on the holiday itself and smooth ramp
		// 0->1 on the day before the holiday and 1->0 on the day after the holiday.

		sep := NewScalerEncoderParams(params.HolidayWidth, 0, 1)
		sep.Name = "holiday"
		sep.Radius = params.HolidayRadius
		de.holidayEncoder = NewScalerEncoder(sep)
		de.holidayOffset = de.width
		de.width += de.holidayEncoder.N
	}

	if params.TimeOfDayWidth > 0 {
		// Value is time of day in hours
		// Radius = 4 hours, e.g. morning, afternoon, evening, early night,
		// late night, etc.

		sep := NewScalerEncoderParams(params.TimeOfDayWidth, 0, 24)
		sep.Name = "time of day"
		sep.Radius = params.TimeOfDayRadius
		sep.Periodic = true
		de.timeOfDayEncoder = NewScalerEncoder(sep)
		de.timeOfDayOffset = de.width
		de.width += de.timeOfDayEncoder.N

	}

	return de
}

/*
	get season scaler from time
*/
func (de *DateEncoder) getSeasonScaler(date time.Time) float64 {
	if de.seasonEncoder == nil {
		return 0.0
	}

	//make year 0 based
	dayOfYear := float64(date.YearDay() - 1)
	return dayOfYear

}

/*
	get day of week scaler from time
*/
func (de *DateEncoder) getDayOfWeekScaler(date time.Time) float64 {
	if de.dayOfWeekEncoder == nil {
		return 0.0
	}
	return float64(date.Weekday())
}

/*
	get weekend scaler from time
*/
func (de *DateEncoder) getWeekendScaler(date time.Time) float64 {
	if de.weekendEncoder == nil {
		return 0.0
	}
	dayOfWeek := date.Weekday()
	timeOfDay := date.Hour() + date.Minute()/60.0

	// saturday, sunday or friday evening
	weekend := 0.0
	if dayOfWeek == time.Saturday ||
		dayOfWeek == time.Sunday ||
		(dayOfWeek == time.Friday && timeOfDay > 18) {
		weekend = 1.0
	}
	return weekend
}

/*
	get holiday scaler from time
*/
func (de *DateEncoder) getHolidayScaler(date time.Time) float64 {
	if de.holidayEncoder == nil {
		return 0.0
	}
	// A "continuous" binary value. = 1 on the holiday itself and smooth ramp
	// 0->1 on the day before the holiday and 1->0 on the day after the holiday.
	// Currently the only holiday we know about is December 25
	// holidays is a list of holidays that occur on a fixed date every year
	val := 0.0

	for _, h := range de.Holidays {
		// hdate is midnight on the holiday
		hDate := time.Date(date.Year(), time.Month(h.A), h.B, 0, 0, 0, 0, time.UTC)
		if date.After(hDate) {
			diff := date.Sub(hDate)
			if (diff/time.Hour)/24 == 0 {
				val = 1
				break
			} else if (diff/time.Hour)/24 == 1 {
				// ramp smoothly from 1 -> 0 on the next day
				val = 1.0 - (float64(diff/time.Second) / (86400))
				break
			}
		} else {
			diff := hDate.Sub(date)
			if (diff/time.Hour)/24 == 1 {
				// ramp smoothly from 0 -> 1 on the previous day
				val = 1.0 - (float64(diff/time.Second) / 86400)
			}

		}
	}

	return val

}

/*

*/
func (de *DateEncoder) getTimeOfDayScaler(date time.Time) float64 {
	if de.timeOfDayEncoder == nil {
		return 0.0
	}
	return float64(date.Hour()) + (float64(date.Minute()) / 60.0)

}

/*
	Encodes input to specifed slice
*/
func (de *DateEncoder) EncodeToSlice(date time.Time, output []bool) {

	learn := false

	// Get a scaler value for each subfield and encode it with the
	// appropriate encoder
	if de.seasonEncoder != nil {
		val := de.getSeasonScaler(date)
		de.seasonEncoder.EncodeToSlice(val, learn, output[de.seasonOffset:])
	}

	if de.holidayEncoder != nil {
		val := de.getHolidayScaler(date)
		de.holidayEncoder.EncodeToSlice(val, learn, output[de.holidayOffset:])
	}

	if de.dayOfWeekEncoder != nil {
		val := de.getDayOfWeekScaler(date)
		de.dayOfWeekEncoder.EncodeToSlice(val, learn, output[de.dayOfWeekOffset:])
	}

	if de.weekendEncoder != nil {
		val := de.getWeekendScaler(date)
		de.weekendEncoder.EncodeToSlice(val, learn, output[de.weekendOffset:])
	}

	if de.timeOfDayEncoder != nil {
		val := de.getTimeOfDayScaler(date)
		de.timeOfDayEncoder.EncodeToSlice(val, learn, output[de.timeOfDayOffset:])
	}

}

/*
	Returns encoded date/time
*/
func (de *DateEncoder) Encode(date time.Time) []bool {
	output := make([]bool, de.width)
	de.EncodeToSlice(date, output)
	return output
}

/*
 Encoder description
*/
func (de *DateEncoder) Description(date time.Time) string {
	return fmt.Sprintf("season %v ", de.seasonOffset) +
		fmt.Sprintf(" day of week: %v", de.dayOfWeekOffset) +
		fmt.Sprintf(" weekend: %v", de.weekendOffset) +
		fmt.Sprintf(" holiday %v", de.holidayOffset) +
		fmt.Sprintf(" time of day: %v ", de.timeOfDayOffset)
}
