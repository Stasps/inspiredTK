// Package util - слой Utils: целочисленная арифметика симуляции.
package util

import "math"

// MilliPerUnit - число милли-единиц в одной целой единице ресурса.
// Все количества в состоянии хранятся в милли-единицах (GAME/save-structure.md).
const MilliPerUnit int64 = 1000

// MilliPerHour - милли-секундная точность тарифа «в час».
const secondsPerHour int64 = 3600

// UnitsToMilli переводит целые единицы в милли-единицы.
func UnitsToMilli(units int64) int64 {
	return units * MilliPerUnit
}

// PerHourToMilli переводит тариф «единиц в час» в милли-единицах в час.
func PerHourToMilli(perHour int64) int64 {
	return perHour * MilliPerUnit
}

// AccrueMilli вычисляет прирост в милли-единицах за elapsedSec при тарифе
// rateMilliPerHour. Целочисленное floor-округление - детерминированно.
func AccrueMilli(rateMilliPerHour, elapsedSec int64) int64 {
	if rateMilliPerHour <= 0 || elapsedSec <= 0 {
		return 0
	}
	return rateMilliPerHour * elapsedSec / secondsPerHour
}

// DistanceMilli возвращает евклидово расстояние между клетками в милли-клетках.
// Сетка квадратная (GAME/game-design.md, «Карта мира»); для координат карты
// промежуточные значения не выходят за 2^53, округление floor детерминировано.
func DistanceMilli(x1, y1, x2, y2 int) int64 {
	dx := int64(x1 - x2)
	dy := int64(y1 - y2)
	d2 := dx*dx + dy*dy
	return int64(math.Sqrt(float64(d2) * 1_000_000))
}

// TravelSeconds вычисляет время марша в секундах при скорости cellsPerHour.
func TravelSeconds(distMilli, cellsPerHour int64) int64 {
	if cellsPerHour <= 0 {
		return 0
	}
	secs := distMilli * secondsPerHour / (cellsPerHour * MilliPerUnit)
	if secs < 1 {
		secs = 1
	}
	return secs
}
