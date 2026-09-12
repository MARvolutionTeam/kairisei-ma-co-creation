package multiplayer

import (
	"fmt"
	"strings"
)

// CardCsvData's CARD_PASSIVE (column 33) is separate from the support-deck
// skill. CN's own family only produces BEGINNING_DRAW (9974f -> buff 412), but
// the JP masters also ship permanent "常時発動" passives, so the shape is
// classified instead of assumed. Resolve the actual support master; never infer
// this from rarity, character name or a hard-coded card-ID list.
type cardPassiveKind int

const (
	cardPassiveNone cardPassiveKind = iota
	cardPassiveBeginningDraw
	cardPassivePermanent
)

// cardPassiveShape resolves and classifies a main card's passive. Startup
// validation walks every card through this, so a shape the engine cannot run
// still fails before a live battle reaches it.
func (catalog *CombatCatalog) cardPassiveShape(card CombatCardDefinition) (cardPassiveKind, CombatSkillDefinition, []CombatSkillRole, error) {
	if card.PassiveSkillID == 0 {
		return cardPassiveNone, CombatSkillDefinition{}, nil, nil
	}
	variants := catalog.SupportSkills[card.PassiveSkillID]
	if len(variants) != 1 {
		return cardPassiveNone, CombatSkillDefinition{}, nil,
			fmt.Errorf("card %d passive %d must have exactly one definition", card.ID, card.PassiveSkillID)
	}
	skill := variants[0]
	// JP passives carry the ARTHUR-job gate as the first branch condition
	// ("SELF_ARTHUR_TYPE" + the job name). Anything else is still refused so an
	// unevaluated condition can never silently apply.
	if skill.BranchCondition2 != "" {
		return cardPassiveNone, skill, nil,
			fmt.Errorf("card %d passive %d has unsupported secondary branch %q", card.ID, card.PassiveSkillID, skill.BranchCondition2)
	}
	switch skill.BranchCondition {
	case "", "SELF_ARTHUR_TYPE":
	default:
		return cardPassiveNone, skill, nil,
			fmt.Errorf("card %d passive %d has unsupported branch %q", card.ID, card.PassiveSkillID, skill.BranchCondition)
	}
	if skill.Target != "SELF" && skill.Target != "USER_ALL" {
		return cardPassiveNone, skill, nil,
			fmt.Errorf("card %d passive %d has unsupported target %q", card.ID, card.PassiveSkillID, skill.Target)
	}
	roles := catalog.SupportSkillRoles[skill.FunctionID]
	if len(roles) == 0 {
		return cardPassiveNone, skill, nil,
			fmt.Errorf("card %d passive %d has no roles", card.ID, card.PassiveSkillID)
	}
	if len(roles) == 1 && roles[0].Function == "BEGINNING_DRAW" {
		if roles[0].Target != "SELECT" || roles[0].ExcludeSelf {
			return cardPassiveNone, skill, roles,
				fmt.Errorf("card %d passive %d has unsupported BEGINNING_DRAW role", card.ID, card.PassiveSkillID)
		}
		return cardPassiveBeginningDraw, skill, roles, nil
	}
	for _, role := range roles {
		if !sphereSupportFunctionRegistered(role.Function) {
			return cardPassiveNone, skill, roles,
				fmt.Errorf("card %d passive %d role %s is not implemented by the battle engine", card.ID, card.PassiveSkillID, role.Function)
		}
	}
	return cardPassivePermanent, skill, roles, nil
}

func (catalog *CombatCatalog) cardBeginningDraw(card CombatCardDefinition) (bool, error) {
	kind, _, _, err := catalog.cardPassiveShape(card)
	if err != nil {
		return false, err
	}
	return kind == cardPassiveBeginningDraw, nil
}

// 5ea1e runs after each user's EX/chalice passives. 9974f/88a20 records
// BEGINNING_DRAW in PASSIVE with duration zero and binds it to CARD_TYPE.
// It remains in state across turns; only the first-battle shuffle consumes
// its priority. Installing it in later waves must not reshuffle held cards.
func (engine *BattleEngine) executeMainCardPassives(owner *battlePlayer) ([]BattleResult, error) {
	var results []BattleResult
	for _, card := range owner.Deck {
		definition := engine.catalog.Cards[card.CardID]
		kind, skill, roles, err := engine.catalog.cardPassiveShape(definition)
		if err != nil {
			return nil, err
		}
		if kind == cardPassiveNone {
			continue
		}
		if kind == cardPassivePermanent {
			rows, err := engine.executePermanentCardPassive(owner, card, skill, roles)
			if err != nil {
				return nil, err
			}
			results = append(results, rows...)
			continue
		}
		role := roles[0]
		header, err := engine.playerCardSkillResult(battleAction{
			memberType: owner.MemberType, cardType: card.CardType, cardID: card.CardID,
			cardLevel: card.Level, target: owner.MemberType, skill: skill,
		}, 0)
		if err != nil {
			return nil, err
		}
		header.Args[6], header.Args[10] = 0, 1
		owner.Effects = append(owner.Effects, battleEffect{Function: "BEGINNING_DRAW", ListType: 1,
			Source: owner.MemberType, SourceSkillID: skill.ID, CardType: card.CardType, RoleIndex: role.RoleIndex, AppliedTurn: engine.turn})
		// 88a20's PASSIVE registration emits the base query before the
		// deferred 69/6 pair, even when BEGINNING_DRAW changes no parameter.
		results = append(results, engine.projectSkillStatusResults([]BattleResult{header, playerBaseParameterResult(owner), battleBuffResultWithListType(owner.MemberType, role.RoleIndex, 1,
			battleBuffCodes["BEGINNING_DRAW"], 0, 1, 0, 0, 0, 0)})...)
		engine.nativeSkillSerial++
		display, err := engine.refreshPassiveDisplayPowers()
		if err != nil {
			return nil, err
		}
		results = append(results, display...)
	}
	return results, nil
}

// executePermanentCardPassive installs a JP "常時発動" main-card passive.
// It deliberately mirrors executePlayerSupportPassives: the same header, the
// same per-role projection and the same durable PASSIVE-list effects, so the
// client's passive and parameter panels stay consistent with EX support cards.
func (engine *BattleEngine) executePermanentCardPassive(owner *battlePlayer, card BattleCard, skill CombatSkillDefinition, roles []CombatSkillRole) ([]BattleResult, error) {
	if !playerMatchesArthurBranch(owner, skill) {
		return nil, nil
	}
	target := owner.MemberType
	if strings.EqualFold(skill.Target, "USER_ALL") {
		target = 0
	}
	header, err := engine.playerCardSkillResult(battleAction{
		memberType: owner.MemberType, cardType: card.CardType, cardID: card.CardID,
		cardLevel: card.Level, target: target, skill: skill,
	}, 0)
	if err != nil {
		return nil, err
	}
	header.Args[6], header.Args[10] = 0, 1 // native cut-in=0, passive/counter flag=1
	results := []BattleResult{header}
	var statusResults []BattleResult
	for _, role := range roles {
		role.SourceSkillID = skill.ID
		effect, err := sphereSupportEffect(role, card.Level, engine.turn, owner.MemberType)
		if err != nil {
			return nil, fmt.Errorf("card %d passive %d role %s: %w", card.CardID, skill.ID, role.Function, err)
		}
		code := battleBuffCodes[role.Function]
		for targetIndex := range engine.players {
			member := &engine.players[targetIndex]
			if member.HP <= 0 || member.GameOver || (target != 0 && target != member.MemberType) ||
				!combatRoleAllowsTarget(role, owner.MemberType, member.MemberType, member.Attribute) {
				continue
			}
			member.Effects = append(member.Effects, effect)
			// Parameter passives must also move the live battle parameter; the
			// damage side only reads the retained Delta list, so a MAX_HP buff
			// would otherwise show nothing until the next recompute.
			if effect.Parameter != "" {
				refreshAppliedPlayerParameter(member, effect.Parameter, effect.Delta)
				results = append(results, playerBaseParameterResult(member))
			}
			statusResults = append(statusResults, battleBuffResultWithListType(member.MemberType, role.RoleIndex, 1,
				code, sphereSupportParameterFlags(effect), sphereSupportAttributeFlags(effect.Attribute), 0, 0, 0, 0))
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

// playerMatchesArthurBranch evaluates the SELF_ARTHUR_TYPE gate JP card
// passives carry (for example "SINGER" for the Millionaire Knights core).
// Startup validation refuses every other branch condition, so this only has to
// evaluate that one.
func playerMatchesArthurBranch(owner *battlePlayer, skill CombatSkillDefinition) bool {
	if !strings.EqualFold(skill.BranchCondition, "SELF_ARTHUR_TYPE") {
		return true
	}
	return owner.ArthurType == arthurTypeCode(skill.BranchParameters[0])
}

// arthurTypeCode mirrors the client's ARTHUR_TYPE enum
// (0 NULL, 1 MERCENARY, 2 MILLIONAIRE, 3 THIEF, 4 SINGER).
func arthurTypeCode(name string) int {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "MERCENARY":
		return 1
	case "MILLIONAIRE":
		return 2
	case "THIEF":
		return 3
	case "SINGER":
		return 4
	default:
		return 0
	}
}

// FUN_0004a66e starts with deck/card-type order, pulls the first five eligible
// cards into a fixed prefix, then area_shuffle only shuffles the suffix.
// Unlike ordinary shuffle, area_shuffle consumes the final modulo-one draw.
// This entry is ONLY used for the first battle: 5db7a sets the 0x44 latch;
// subsequent waves and depleted-pool reshuffles use ordinary shuffle.
func (engine *BattleEngine) shuffleOpeningDeck(player *battlePlayer, guaranteed [10]bool) {
	count := 0
	for index := range player.DeckOrder {
		if guaranteed[player.DeckOrder[index]] {
			player.DeckOrder[index], player.DeckOrder[count] = player.DeckOrder[count], player.DeckOrder[index]
			count++
			if count == len(player.Hand) {
				break
			}
		}
	}
	if count == 0 {
		engine.shuffleDeck(player)
		return
	}
	for end := len(player.DeckOrder); count < end; end-- {
		swapIndex := count + int(engine.rng.next()%uint32(end-count))
		player.DeckOrder[end-1], player.DeckOrder[swapIndex] = player.DeckOrder[swapIndex], player.DeckOrder[end-1]
	}
}
