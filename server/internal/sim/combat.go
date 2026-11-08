package sim

import "inspiredtk/server/internal/domain"

// BattleParams - целочисленные параметры боя из конфига (промилле).
type BattleParams struct {
	AttackStat              int64
	DefenseStat             int64
	DefenseModifierPerMille int64
	RaidThresholdPerMille   int64
	RaidFactorPerMille      int64
}

// BattleOutcome - результат боя: потери сторон и победа атакующего.
type BattleOutcome struct {
	AttackerLosses int
	DefenderLosses int
	AttackerWon    bool
}

// battleParams собирает параметры боя из конфигов (юнит MVP-0 - пехотинец,
// стен в MVP-0 нет - модификатор обороны всегда «без стен»).
func (e *Engine) battleParams() BattleParams {
	uc := e.Cfg.UnitByID["infantry"]
	return BattleParams{
		AttackStat:              uc.Attack,
		DefenseStat:             uc.Defense,
		DefenseModifierPerMille: e.Cfg.Combat.DefenseModifierNoWallsPerMille,
		RaidThresholdPerMille:   e.Cfg.Combat.RaidSuperiorityThresholdPerMille,
		RaidFactorPerMille:      e.Cfg.Combat.RaidLossFactorPerMille,
	}
}

// ResolveBattle - детерминированный расчёт боя (GAME/game-design.md,
// «Боевая система»): (Атака x Количество) vs (Защита x Количество x Модификатор).
// Только целочисленная арифметика с floor - одинаковый ввод даёт
// одинаковый результат на любом сервере.
//
// Атака: проигравший теряет 100%, победитель - долю по соотношению сил.
// Набег: при перевесе сильнее порога - как атака; иначе обе стороны теряют
// пропорционально с фактором набега (решение человека; минимум 10% потерь
// пока не применяется).
func ResolveBattle(attacker, defender int, mode domain.MarchType, p BattleParams) BattleOutcome {
	if attacker <= 0 {
		return BattleOutcome{}
	}
	if defender <= 0 {
		return BattleOutcome{AttackerWon: true}
	}
	atk := p.AttackStat * int64(attacker)
	def := p.DefenseStat * int64(defender) * p.DefenseModifierPerMille / 1000

	if atk > def {
		out := BattleOutcome{AttackerWon: true}
		if mode == domain.MarchRaid && !superior(atk, def, p.RaidThresholdPerMille) {
			out.AttackerLosses = proportionalLosses(attacker, def, atk, p.RaidFactorPerMille)
			out.DefenderLosses = proportionalLosses(defender, atk, def, p.RaidFactorPerMille)
			return out
		}
		out.DefenderLosses = defender
		out.AttackerLosses = int(int64(attacker) * def / atk)
		return out
	}

	out := BattleOutcome{AttackerWon: false}
	if mode == domain.MarchRaid && !superior(def, atk, p.RaidThresholdPerMille) {
		out.AttackerLosses = proportionalLosses(attacker, def, atk, p.RaidFactorPerMille)
		out.DefenderLosses = proportionalLosses(defender, atk, def, p.RaidFactorPerMille)
		return out
	}
	out.AttackerLosses = attacker
	out.DefenderLosses = int(int64(defender) * atk / def)
	return out
}

// superior сообщает, что stronger превосходит weaker не слабее порога (промилле).
func superior(stronger, weaker, thresholdPerMille int64) bool {
	return stronger*1000 >= weaker*thresholdPerMille
}

// proportionalLosses - потери стороны при набеге без решающего перевеса:
// count * enemyPower * factor / (ownPower * 1000), не больше всего отряда.
func proportionalLosses(count int, enemyPower, ownPower, factorPerMille int64) int {
	losses := int(int64(count) * enemyPower * factorPerMille / (ownPower * 1000))
	if losses > count {
		losses = count
	}
	return losses
}
