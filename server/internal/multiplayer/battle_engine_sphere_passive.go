package multiplayer

import (
	"fmt"
	"sort"
	"strings"
)

const sphereSupportRateLimit = 3000

// SphereSupportSkill resolves the separate support_skill/support_skill_role
// family referenced by SphrCsvData.passive_skill_id. These definitions are
// not player-card roles: CN libbattle5 installs them from CHALICE slot one
// once during battle setup and keeps them in BATTLE5_BUFFLIST_TYPE.PASSIVE.
func (c *CombatCatalog) SphereSupportSkill(sphereID int) (CombatSkillDefinition, []CombatSkillRole, error) {
	sphere, exists := c.Spheres[sphereID]
	if !exists {
		return CombatSkillDefinition{}, nil, fmt.Errorf("sphere %d is unavailable", sphereID)
	}
	if sphere.PassiveSkillID == 0 {
		return CombatSkillDefinition{}, nil, nil
	}
	variants := c.SupportSkills[sphere.PassiveSkillID]
	if len(variants) == 0 {
		return CombatSkillDefinition{}, nil, fmt.Errorf("sphere %d support skill %d is incomplete", sphereID, sphere.PassiveSkillID)
	}
	roles := c.SupportSkillRoles[variants[0].FunctionID]
	if len(roles) == 0 {
		return CombatSkillDefinition{}, nil, fmt.Errorf("sphere %d support skill role %d is incomplete", sphereID, variants[0].FunctionID)
	}
	return variants[0], append([]CombatSkillRole(nil), roles...), nil
}

// ValidateSphereSupportFunctionCoverage gates only support roles reachable
// through the immutable sphere master. support_skill_role also contains other
// dormant systems, so treating the entire table as active would overstate the
// client's battle surface.
func (c *CombatCatalog) ValidateSphereSupportFunctionCoverage() error {
	missing := make(map[string]struct{})
	references := 0
	for sphereID, sphere := range c.Spheres {
		if sphere.PassiveSkillID == 0 {
			continue
		}
		if sphere.Type != sphereTypeChalice {
			return fmt.Errorf("official sphere %d has support skill %d but is not CHALICE", sphereID, sphere.PassiveSkillID)
		}
		skill, roles, err := c.SphereSupportSkill(sphereID)
		if err != nil {
			return err
		}
		if !strings.EqualFold(skill.Target, "USER_ALL") {
			return fmt.Errorf("official sphere %d support skill %d target is %q, want USER_ALL", sphereID, skill.ID, skill.Target)
		}
		if skill.BranchCondition != "" || skill.BranchCondition2 != "" {
			return fmt.Errorf("official sphere %d support skill %d has unsupported branch conditions", sphereID, skill.ID)
		}
		for _, role := range roles {
			references++
			if !strings.EqualFold(role.Target, "SELECT") {
				return fmt.Errorf("official sphere %d support role %s target is %q, want SELECT", sphereID, role.Function, role.Target)
			}
			if role.ExcludeSelf {
				return fmt.Errorf("official sphere %d support role %s unexpectedly excludes its owner", sphereID, role.Function)
			}
			for index := 7; index < len(role.Parameters); index++ {
				value := strings.TrimSpace(role.Parameters[index])
				if value != "" && value != "0" {
					return fmt.Errorf("official sphere %d support role %s has unsupported p%d=%q", sphereID, role.Function, index+1, value)
				}
			}
			if !sphereSupportFunctionRegistered(role.Function) {
				missing[role.Function] = struct{}{}
				continue
			}
			if _, exists := battleBuffCodes[role.Function]; !exists {
				missing[role.Function+"(buff-code)"] = struct{}{}
			}
			attribute := strings.ToUpper(strings.TrimSpace(role.Parameters[5]))
			if sphereSupportAttributeFlags(attribute) == 0 {
				return fmt.Errorf("official sphere %d support role %s has unsupported attribute %q", sphereID, role.Function, attribute)
			}
			switch role.Function {
			case "ATK_UP_BOOST", "DEF_UP_BOOST", "ATK_BREAK_BOOST":
				if combatParameterFlag(role.Parameters[6]) == 0 {
					return fmt.Errorf("official sphere %d support role %s has unsupported battle parameter %q", sphereID, role.Function, role.Parameters[6])
				}
			case "DAMAGE_BOOST", "DAMAGE_CUT2":
				physics := strings.ToUpper(strings.TrimSpace(role.Parameters[6]))
				if physics != "" && physics != "ALL" && physics != "NULL" && physics != "PHYSICS" && physics != "MAGIC" {
					return fmt.Errorf("official sphere %d support role %s has unsupported physics %q", sphereID, role.Function, physics)
				}
			}
		}
	}
	if references == 0 {
		return fmt.Errorf("official sphere master has no reachable support roles")
	}
	if len(missing) == 0 {
		return nil
	}
	functions := make([]string, 0, len(missing))
	for function := range missing {
		functions = append(functions, function)
	}
	sort.Strings(functions)
	return fmt.Errorf("unregistered official CN sphere support functions: %s", strings.Join(functions, ", "))
}

func sphereSupportFunctionRegistered(function string) bool {
	switch function {
	case "DAMAGE_BOOST", "ATK_UP_BOOST", "DEF_UP_BOOST", "ATK_BREAK_BOOST", "DAMAGE_CUT2", "GUARD_BREAK_BOOST", "HEAL_BOOST", "CRITICAL_BOOST":
		return true
	// JP permanent main-card passives ("常時発動") reuse the active-skill fixed
	// parameter roles. Their arithmetic already exists for player skills, so
	// only the support registration was missing.
	case "ATK_UP_FIXED", "DEF_UP_FIXED":
		return true
	default:
		return false
	}
}

// executeSphereSupportPassives mirrors FUN_0005e700: only BATTLE5_SPHR_SLOT
// one is resolved, its wrapper must be CHALICE, and passive_skill_id is run
// once for USER_ALL before ordinary enemy start passives.
func (engine *BattleEngine) executeSphereSupportPassives() ([]BattleResult, error) {
	results := make([]BattleResult, 0, maxRoomMembers*(maxRoomMembers+1))
	for ownerIndex := range engine.players {
		rows, err := engine.executeSphereSupportPassive(&engine.players[ownerIndex])
		if err != nil {
			return nil, err
		}
		results = append(results, rows...)
	}
	return results, nil
}

func (engine *BattleEngine) executeSphereSupportPassive(owner *battlePlayer) ([]BattleResult, error) {
	var results []BattleResult
	sphere := &owner.Spheres[0]
	if sphere.SphereID == 0 || sphere.Type != sphereTypeChalice || owner.HP <= 0 || owner.GameOver {
		return nil, nil
	}
	definition, exists := engine.catalog.Spheres[sphere.SphereID]
	if !exists || definition.PassiveSkillID == 0 {
		return nil, nil
	}
	skill, roles, err := engine.catalog.SphereSupportSkill(sphere.SphereID)
	if err != nil {
		return nil, err
	}
	header, err := playerSphereSkillResult(battleAction{
		memberType: owner.MemberType,
		sphereSlot: 1,
		cardLevel:  sphere.Level,
		target:     0,
		skill:      skill,
		roles:      roles,
	}, 0)
	if err != nil {
		return nil, err
	}
	header.Args[10] = 1 // Original passive/counter flag, not active sphere use.
	results = append(results, header)
	var statusResults []BattleResult
	for _, role := range roles {
		role.SourceSkillID = skill.ID
		effect, err := sphereSupportEffect(role, sphere.Level, engine.turn, owner.MemberType)
		if err != nil {
			return nil, fmt.Errorf("sphere %d support role %s: %w", sphere.SphereID, role.Function, err)
		}
		code := battleBuffCodes[role.Function]
		for targetIndex := range engine.players {
			target := &engine.players[targetIndex]
			if target.HP <= 0 || target.GameOver || !combatRoleAllowsTarget(role, owner.MemberType, target.MemberType, target.Attribute) {
				continue
			}
			target.Effects = append(target.Effects, effect)
			engine.recordAIStatusApplied(target.MemberType, effect)
			results = append(results, playerBaseParameterResult(target))
			statusResults = append(statusResults, battleBuffResultWithListType(
				target.MemberType,
				role.RoleIndex,
				1,
				code,
				sphereSupportParameterFlags(effect),
				sphereSupportAttributeFlags(effect.Attribute),
				0, 0, 0, 0,
			))
		}
	}
	results = append(results, engine.projectSkillStatusResults(statusResults)...)
	engine.nativeSkillSerial++
	display, err := engine.refreshPassiveDisplayPowers()
	if err != nil {
		return nil, err
	}
	return append(results, display...), nil
}

// 7adb0 refreshes after each individual passive, not after all four owners.
// Its first sphere comparison uses the zero-initialized cache, before 307.
func (engine *BattleEngine) refreshPassiveDisplayPowers() ([]BattleResult, error) {
	for i := range engine.players {
		for j := range engine.players[i].Spheres {
			engine.players[i].Spheres[j].Display.Known = true
		}
	}
	return engine.refreshBattleDisplayPowers()
}

func sphereSupportEffect(role CombatSkillRole, level int, turn int, source int) (battleEffect, error) {
	if !sphereSupportFunctionRegistered(role.Function) {
		return battleEffect{}, fmt.Errorf("unsupported function %q", role.Function)
	}
	effect := battleEffect{
		Function:      role.Function,
		ListType:      1,
		Attribute:     strings.ToUpper(strings.TrimSpace(role.Parameters[5])),
		Kind:          1,
		Remaining:     maxInt(1, combatParameterInt(role.Parameters[0])),
		AppliedTurn:   turn,
		Source:        source,
		SourceSkillID: role.SourceSkillID,
		RoleIndex:     role.RoleIndex,
		CostMin:       combatParameterInt(role.Parameters[7]),
		CostMax:       combatParameterInt(role.Parameters[8]),
	}
	switch role.Function {
	case "DAMAGE_BOOST":
		effect.Rate = combatParameterInt(role.Parameters[1]) + combatParameterInt(role.Parameters[2])*level
		effect.Value = combatParameterInt(role.Parameters[3]) + combatParameterInt(role.Parameters[4])*level/1000
		effect.DamageKind = strings.ToUpper(strings.TrimSpace(role.Parameters[6]))
	case "ATK_UP_BOOST", "DEF_UP_BOOST", "ATK_BREAK_BOOST", "GUARD_BREAK_BOOST":
		effect.Value = combatParameterInt(role.Parameters[1]) + combatParameterInt(role.Parameters[2])*level/1000
		effect.Rate = combatParameterInt(role.Parameters[3]) + combatParameterInt(role.Parameters[4])*level
		effect.Parameter = strings.ToUpper(strings.TrimSpace(role.Parameters[6]))
	case "HEAL_BOOST":
		effect.Value = combatParameterInt(role.Parameters[1]) + combatParameterInt(role.Parameters[2])*level/1000
		effect.Rate = combatParameterInt(role.Parameters[3]) + combatParameterInt(role.Parameters[4])*level
		effect.CostMin, effect.CostMax = combatParameterInt(role.Parameters[6]), combatParameterInt(role.Parameters[7])
	case "CRITICAL_BOOST":
		effect.Rate = combatParameterInt(role.Parameters[1]) + combatParameterInt(role.Parameters[2])*level
		effect.Attribute = strings.ToUpper(strings.TrimSpace(role.Parameters[3]))
		effect.DamageKind = strings.ToUpper(strings.TrimSpace(role.Parameters[4]))
		effect.CostMin, effect.CostMax = combatParameterInt(role.Parameters[5]), combatParameterInt(role.Parameters[6])
	case "DAMAGE_CUT2":
		effect.Value = combatParameterInt(role.Parameters[1]) + combatParameterInt(role.Parameters[2])*level/1000
		effect.Rate = combatParameterInt(role.Parameters[3]) + combatParameterInt(role.Parameters[4])*level
		effect.DamageKind = strings.ToUpper(strings.TrimSpace(role.Parameters[6]))
	case "ATK_UP_FIXED", "DEF_UP_FIXED":
		// Same fixed-buff segments as the active-skill path, minus the chain
		// bonus a passive never has. Keep the int32 intermediate so the native
		// overflow contract (see battle_fixed_parameters_test.go) is preserved.
		first, second := fixedBuffRoleSegments(role, level)
		effect.Parameter = strings.ToUpper(strings.TrimSpace(role.Parameters[1]))
		effect.Value = int(first + second)
		effect.Delta = effect.Value
	}
	return effect, nil
}

func sphereSupportParameterFlags(_ battleEffect) int {
	// Parameter matching remains in the effect. Native support registration
	// does not project it as an immediate ATK/DEF panel-change flag in row 69.
	return 0
}

func sphereSupportAttributeFlags(attribute string) int {
	codes, valid := sphereSupportAttributeCodes(attribute)
	if !valid {
		return 0
	}
	code := codes[0]
	if len(codes) == 2 {
		code = codes[0]*100 + codes[1]
	}
	// 88a20 projects the raw ATTR enum with an x86 shift. The component
	// mask used by attribute matching is a separate native operation.
	return int(uint32(1) << uint(code&31))
}

// Native FUN_000ceb85 splits the decimal pair encoding used by ATTR
// (for example LIGHT_DARK=405) and FUN_000cecc3 projects one bit for each
// component. attr_cast only accepts the ten ordered pairs among FIRE..DARK.
func sphereSupportAttributeCodes(attribute string) ([]int, bool) {
	normalized := strings.ToUpper(strings.TrimSpace(attribute))
	if normalized == "" || normalized == "ALL" || normalized == "NULL" || normalized == "NONE" {
		return []int{0}, true
	}
	parts := strings.FieldsFunc(normalized, func(character rune) bool {
		return character == '_' || character == '/'
	})
	if len(parts) == 0 || len(parts) > 2 {
		return nil, false
	}
	codes := make([]int, 0, len(parts))
	for _, part := range parts {
		code := combatAttributeCode(part)
		if code == 0 {
			return nil, false
		}
		codes = append(codes, code)
	}
	if len(codes) == 2 && (codes[0] < 1 || codes[0] > 5 || codes[1] <= codes[0] || codes[1] > 5) {
		return nil, false
	}
	return codes, true
}

func sphereSupportBoostedValue(effects []battleEffect, function string, attribute string, parameter string, value int, costs ...int) int {
	fixed, rate := sphereSupportBoostTerms(effects, function, attribute, parameter, costs...)
	// 88a20/8fb80 use signed int32 multiply then divide, not a 64-bit product.
	return int((int32(value+fixed) * int32(1000+rate)) / 1000)
}

func sphereSupportBoostTerms(effects []battleEffect, function string, attribute string, parameter string, costs ...int) (int, int) {
	cost := 0
	cardType := 0
	if len(costs) > 0 {
		cost = costs[0]
	}
	if len(costs) > 1 {
		cardType = costs[1]
	}
	fixed := 0
	rate := 0
	for _, effect := range effects {
		// 46252 visits lists0..6 plus card-owned list7. Membership, not a
		// second duration check, determines whether a retained state applies.
		if effect.Function != function || effect.ListType < 0 || effect.ListType > 7 ||
			(effect.ListType == 7 && (cardType == 0 || (effect.CardType != 0 && effect.CardType != cardType))) ||
			!supportAttributeMatches(effect.Attribute, attribute) || !supportCostMatches(effect, cost) {
			continue
		}
		if parameter != "" && !strings.EqualFold(effect.Parameter, parameter) {
			continue
		}
		fixed += effect.Value
		rate += effect.Rate
	}
	// 4641c/88a20 sum parameter-boost rates without the 71d48 damage cap.
	return fixed, rate
}

func sphereDamageBoost(effects []battleEffect, attribute string, physics string, value int, costs ...int) int {
	fixed, rate := sphereDamageBoostTerms(effects, attribute, physics, costs...)
	return int((int64(value+fixed) * int64(1000+rate)) / 1000)
}

func sphereDamageBoostTerms(effects []battleEffect, attribute string, physics string, costs ...int) (int, int) {
	cost := 0
	if len(costs) > 0 {
		cost = costs[0]
	}
	fixed := 0
	rate := 0
	for _, effect := range effects {
		if effect.Function != "DAMAGE_BOOST" || effect.ListType != 1 || effect.Remaining <= 0 ||
			!supportAttributeMatches(effect.Attribute, attribute) || !damagePhysicsMatches(effect.DamageKind, physics) || !supportCostMatches(effect, cost) {
			continue
		}
		fixed += effect.Value
		rate += effect.Rate
	}
	rate = maxInt(0, minInt(sphereSupportRateLimit, rate))
	return fixed, rate
}

func sphereDamageCut(effects []battleEffect, attribute string, physics string, damage int) int {
	if damage <= 0 {
		return 0
	}
	fixed := 0
	rate := 0
	for _, effect := range effects {
		if effect.Function != "DAMAGE_CUT2" || effect.ListType != 1 || effect.Remaining <= 0 ||
			!damageAttributeMatches(effect.Attribute, attribute) || !damagePhysicsMatches(effect.DamageKind, physics) {
			continue
		}
		fixed += effect.Value
		rate += effect.Rate
	}
	rate = maxInt(0, minInt(sphereSupportRateLimit, rate))
	remaining := maxInt(0, damage-fixed)
	remaining = int((int64(remaining) * int64(maxInt(0, 1000-rate))) / 1000)
	return maxInt(1, remaining)
}
