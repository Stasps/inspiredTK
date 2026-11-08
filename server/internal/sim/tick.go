package sim

import "inspiredtk/server/internal/util"

// distanceCells и travelTime - тонкие обёртки над util, чтобы пакет sim
// не таскал util в каждый файл.
func distanceCells(x1, y1, x2, y2 int) int64 {
	return util.DistanceMilli(x1, y1, x2, y2)
}

func travelTime(distMilli, cellsPerHour int64) int64 {
	return util.TravelSeconds(distMilli, cellsPerHour)
}

// Tick - логический тик симуляции (1 секунда): завершение строек и
// тренировок, прибытия маршей, планировщик варваров. Начисление ресурсов
// здесь не происходит - оно ленивое, по формуле от прошедшего времени.
// Parameters:
//   - now: серверное время тика (единственный источник времени)
func (e *Engine) Tick(now int64) {
	e.World.Tick++
	for _, id := range sortedKeys(e.World.Villages) {
		v := e.World.Villages[id]
		e.applyConstruction(v, now)
		e.applyTraining(v, now)
	}
	e.resolveDueMarches(now)
	e.tickBarbarians(now)
}
