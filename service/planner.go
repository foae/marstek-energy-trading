package service

import (
	"math"
	"sort"
	"time"

	"github.com/shopspring/decimal"
)

const plannerEnergyEpsilon = 0.000001

type chargeAllocation struct {
	// Windows contains every eligible slice. The selected prefix has Count
	// price-sorted elements; its final window may be truncated to the exact
	// requested energy.
	Windows     []TimeWindow
	Count       int
	ReservedKWh float64
	Cost        decimal.Decimal
	Feasible    bool
}

// allocateCheapestChargeSlices selects exactly the required AC energy from
// eligible tariff slices. It deliberately knows nothing about planning: the
// service uses the same allocator when SOC changes, so the planner's price and
// partial-slice assumptions are executable rather than aspirational.
func allocateCheapestChargeSlices(windows []TimeWindow, requiredKWh, powerKW float64) chargeAllocation {
	owned := append([]TimeWindow(nil), windows...)
	sort.SliceStable(owned, func(i, j int) bool {
		if owned[i].Price.Equal(owned[j].Price) {
			return owned[i].Start.Before(owned[j].Start)
		}
		return owned[i].Price.LessThan(owned[j].Price)
	})
	return allocateCheapestChargeSlicesOwned(owned, requiredKWh, powerKW)
}

// allocateCheapestChargeSlicesOwned consumes price-sorted windows. Callers
// retain ownership of the input only by passing a copy.
func allocateCheapestChargeSlicesOwned(windows []TimeWindow, requiredKWh, powerKW float64) chargeAllocation {
	result := chargeAllocation{Windows: windows}
	if requiredKWh <= plannerEnergyEpsilon {
		result.Feasible = true
		return result
	}
	if powerKW <= 0 {
		return result
	}

	remaining := requiredKWh
	for i := range result.Windows {
		if remaining <= plannerEnergyEpsilon {
			break
		}
		window := &result.Windows[i]
		duration := window.End.Sub(window.Start)
		if duration <= minimumAutomaticControlWindow {
			continue
		}
		energy := powerKW * duration.Hours()
		if energy <= 0 {
			continue
		}
		if energy > remaining {
			duration = time.Duration(math.Round(remaining / powerKW * float64(time.Hour)))
			if duration <= minimumAutomaticControlWindow {
				if !reserveExecutableShortChargeTail(&result, i, remaining, powerKW, energy) {
					continue
				}
				remaining = 0
				break
			}
			energy = remaining
		}
		if i != result.Count {
			result.Windows[result.Count], result.Windows[i] = result.Windows[i], result.Windows[result.Count]
			window = &result.Windows[result.Count]
		}
		if energy < powerKW*window.End.Sub(window.Start).Hours() {
			window.End = window.Start.Add(duration)
		}
		result.ReservedKWh += energy
		result.Cost = result.Cost.Add(window.Price.Mul(decimal.NewFromFloat(energy)))
		remaining -= energy
		result.Count++
	}
	result.Feasible = remaining <= plannerEnergyEpsilon
	return result
}

type chargeSliceAdjustment struct {
	index int
	drop  bool
	end   time.Time
}

// reserveExecutableShortChargeTail makes a sub-minimum final remainder
// executable without changing the required energy. It borrows from the most
// expensive selected slices, retaining executable prefixes where possible.
func reserveExecutableShortChargeTail(result *chargeAllocation, candidateIndex int, remaining, powerKW, candidateEnergy float64) bool {
	finalEnergy := remaining
	minimumDuration := minimumAutomaticControlWindow + time.Second
	minimumEnergy := powerKW * minimumDuration.Hours()
	adjustments := make([]chargeSliceAdjustment, 0, result.Count)

	for i := result.Count - 1; i >= 0 && finalEnergy < minimumEnergy; i-- {
		window := result.Windows[i]
		energy := powerKW * window.End.Sub(window.Start).Hours()
		needed := minimumEnergy - finalEnergy
		retainedDuration := window.End.Sub(window.Start) - time.Duration(math.Round(needed/powerKW*float64(time.Hour)))
		if retainedDuration > minimumAutomaticControlWindow {
			adjustments = append(adjustments, chargeSliceAdjustment{
				index: i,
				end:   window.Start.Add(retainedDuration),
			})
			finalEnergy += needed
			break
		}
		adjustments = append(adjustments, chargeSliceAdjustment{index: i, drop: true})
		finalEnergy += energy
	}
	if finalEnergy < minimumEnergy || finalEnergy > candidateEnergy {
		return false
	}

	for _, adjustment := range adjustments {
		if adjustment.drop {
			copy(result.Windows[adjustment.index:result.Count-1], result.Windows[adjustment.index+1:result.Count])
			result.Count--
			continue
		}
		result.Windows[adjustment.index].End = adjustment.end
	}
	if candidateIndex != result.Count {
		result.Windows[result.Count], result.Windows[candidateIndex] = result.Windows[candidateIndex], result.Windows[result.Count]
	}
	result.Windows[result.Count].End = result.Windows[result.Count].Start.Add(
		time.Duration(math.Round(finalEnergy / powerKW * float64(time.Hour))),
	)
	result.Count++
	recalculateChargeAllocation(result, powerKW)
	return true
}

func recalculateChargeAllocation(result *chargeAllocation, powerKW float64) {
	result.ReservedKWh = 0
	result.Cost = decimal.Zero
	for _, window := range result.Windows[:result.Count] {
		energy := powerKW * window.End.Sub(window.Start).Hours()
		result.ReservedKWh += energy
		result.Cost = result.Cost.Add(window.Price.Mul(decimal.NewFromFloat(energy)))
	}
}

type valuePlan struct {
	cycles   []TradeCycle
	value    decimal.Decimal
	sessions int
}

type gridMemoKey struct {
	earliestNanos int64
	cyclesLeft    int
	initialDC     uint64
}

type chargeAllocationMemoKey struct {
	earliestNanos  int64
	dischargeStart int
	requiredAC     uint64
}

type dischargeCandidate struct {
	window              TimeWindow
	revenue             decimal.Decimal
	feasible            bool
	eligibleChargeSlots []priceSlot
}

type gridPlanner struct {
	slots               []priceSlot
	dischargeCandidates []dischargeCandidate
	cfg                 AnalyzerConfig
	chargeEfficiency    float64
	roundTrip           float64
	usableDCKWh         float64
	fullDeliveryKWh     float64
	chargePowerKW       float64
	dischargePowerKW    float64
	planningStart       time.Time
	memo                map[gridMemoKey]valuePlan
	allocationMemo      map[chargeAllocationMemoKey]chargeAllocation
}

func newGridPlanner(slots []priceSlot, cfg AnalyzerConfig) *gridPlanner {
	chargeEfficiency := cfg.ChargeEfficiency
	if !(chargeEfficiency > 0 && chargeEfficiency <= 1) {
		chargeEfficiency = 1
	}
	roundTrip := cfg.Efficiency
	if !(roundTrip > 0 && roundTrip <= 1) {
		roundTrip = 1
	}
	chargePowerKW := float64(cfg.ChargePowerW) / 1000
	dischargePowerKW := float64(cfg.DischargePowerW) / 1000
	if chargePowerKW <= 0 {
		chargePowerKW = 2.5
	}
	if dischargePowerKW <= 0 {
		dischargePowerKW = 2.5
	}
	usableDCKWh := cfg.BatteryCapacityKWh * (1 - cfg.BatteryMinSOC)
	if usableDCKWh < 0 {
		usableDCKWh = 0
	}
	planningStart := cfg.Now
	if planningStart.IsZero() && len(slots) > 0 {
		planningStart = slots[0].Time
	}
	chargeSlots := append([]priceSlot(nil), slots...)
	sort.SliceStable(chargeSlots, func(i, j int) bool {
		if chargeSlots[i].Value.Equal(chargeSlots[j].Value) {
			return chargeSlots[i].Time.Before(chargeSlots[j].Time)
		}
		return chargeSlots[i].Value.LessThan(chargeSlots[j].Value)
	})
	planner := &gridPlanner{
		slots:            slots,
		cfg:              cfg,
		chargeEfficiency: chargeEfficiency,
		roundTrip:        roundTrip,
		usableDCKWh:      usableDCKWh,
		fullDeliveryKWh:  usableDCKWh * roundTrip / chargeEfficiency,
		chargePowerKW:    chargePowerKW,
		dischargePowerKW: dischargePowerKW,
		planningStart:    planningStart,
		memo:             make(map[gridMemoKey]valuePlan),
		allocationMemo:   make(map[chargeAllocationMemoKey]chargeAllocation),
	}
	planner.dischargeCandidates = make([]dischargeCandidate, len(slots))
	for start := range slots {
		window, revenue, ok := planner.fullDischargeCandidate(start)
		planner.dischargeCandidates[start] = dischargeCandidate{
			window:   window,
			revenue:  revenue,
			feasible: ok,
		}
		if !ok {
			continue
		}
		// Eligibility depends on the paired discharge, not the inventory-sale
		// endpoint. Price it once instead of repeating decimal comparisons for
		// every candidate initial-energy and earliest-charge combination.
		maxPrice := window.Price.Mul(decimal.NewFromFloat(roundTrip)).Sub(decimal.NewFromFloat(cfg.MinPriceSpread))
		for _, slot := range chargeSlots {
			if slot.Time.Before(window.Start) && slot.Time.Add(15*time.Minute).After(planningStart) &&
				chargePriceEligible(slot.Value, maxPrice, cfg.MinPriceSpread) {
				planner.dischargeCandidates[start].eligibleChargeSlots = append(planner.dischargeCandidates[start].eligibleChargeSlots, slot)
			}
		}
	}
	return planner
}

func (p *gridPlanner) best(earliest time.Time, initialDCKWh float64, cyclesLeft int) valuePlan {
	if cyclesLeft <= 0 || p.usableDCKWh <= plannerEnergyEpsilon || p.fullDeliveryKWh <= plannerEnergyEpsilon {
		return valuePlan{}
	}
	if earliest.Before(p.planningStart) {
		earliest = p.planningStart
	}
	key := gridMemoKey{earliestNanos: earliest.UnixNano(), cyclesLeft: cyclesLeft, initialDC: math.Float64bits(initialDCKWh)}
	if cached, ok := p.memo[key]; ok {
		return cached
	}

	requiredDC := p.usableDCKWh - initialDCKWh
	if requiredDC <= plannerEnergyEpsilon {
		// A completely inventory-backed discharge is evaluated by the
		// inventory-sale alternative, not represented as a grid cycle.
		return valuePlan{}
	}
	requiredAC := requiredDC / p.chargeEfficiency
	best := valuePlan{}
	for start, candidate := range p.dischargeCandidates {
		if !candidate.feasible || candidate.window.Start.Before(earliest) {
			continue
		}
		allocation := p.chargeAllocation(earliest, start, requiredAC)
		option := p.cycleFromAllocation(start, allocation, cyclesLeft)
		if betterValuePlan(option, best) {
			best = option
		}
	}
	p.memo[key] = best
	return best
}

// cycleFromAllocation evaluates one first discharge without caching it as an
// unrestricted best() result. Interior inventory endpoints share its suffix,
// but must not poison the memo with a restricted first-discharge choice.
func (p *gridPlanner) cycleFromAllocation(start int, allocation chargeAllocation, cyclesLeft int) valuePlan {
	if !allocation.Feasible || allocation.Count == 0 {
		return valuePlan{}
	}
	candidate := p.dischargeCandidates[start]
	value := candidate.revenue.Sub(allocation.Cost)
	if !value.IsPositive() {
		return valuePlan{}
	}
	chargeStart := allocation.Windows[0].Start
	for _, window := range allocation.Windows[1:allocation.Count] {
		if window.Start.Before(chargeStart) {
			chargeStart = window.Start
		}
	}
	cycle := TradeCycle{
		ChargeWindow: TimeWindow{
			Start: chargeStart,
			End:   candidate.window.Start,
			Price: weightedPrice(allocation.Cost, allocation.ReservedKWh),
		},
		DischargeWindow: candidate.window,
		Profit:          value.Div(decimal.NewFromFloat(allocation.ReservedKWh)),
	}
	next := p.best(candidate.window.End, 0, cyclesLeft-1)
	return valuePlan{
		cycles:   append([]TradeCycle{cycle}, next.cycles...),
		value:    value.Add(next.value),
		sessions: controlSessionCount(allocation.Windows[:allocation.Count]) + 1 + next.sessions,
	}
}

func (p *gridPlanner) chargeAllocation(earliest time.Time, dischargeStart int, requiredKWh float64) chargeAllocation {
	key := chargeAllocationMemoKey{
		earliestNanos:  earliest.UnixNano(),
		dischargeStart: dischargeStart,
		requiredAC:     math.Float64bits(requiredKWh),
	}
	if cached, ok := p.allocationMemo[key]; ok {
		return cached
	}
	allocation := p.allocateChargeSlices(earliest, dischargeStart, requiredKWh, nil)
	p.allocationMemo[key] = allocation
	return allocation
}

// Interior endpoints are one-use alternatives: reuse their window storage,
// rather than retaining every floating-point demand in allocationMemo.
func (p *gridPlanner) allocateChargeSlices(earliest time.Time, dischargeStart int, requiredKWh float64, scratch []TimeWindow) chargeAllocation {
	candidate := p.dischargeCandidates[dischargeStart]
	windows := scratch[:0]
	if cap(windows) < len(candidate.eligibleChargeSlots) {
		windows = make([]TimeWindow, 0, len(candidate.eligibleChargeSlots))
	}
	for _, slot := range candidate.eligibleChargeSlots {
		start, end := slot.Time, slot.Time.Add(15*time.Minute)
		if start.Before(earliest) {
			start = earliest
		}
		if end.After(candidate.window.Start) {
			end = candidate.window.Start
		}
		if !start.Before(end) {
			continue
		}
		windows = append(windows, TimeWindow{Start: start, End: end, Price: slot.Value})
	}
	return allocateCheapestChargeSlicesOwned(windows, requiredKWh, p.chargePowerKW)
}

func (p *gridPlanner) fullDischargeCandidate(start int) (TimeWindow, decimal.Decimal, bool) {
	if start >= len(p.slots) {
		return TimeWindow{}, decimal.Zero, false
	}
	controlCutoff := p.cfg.Now
	if !controlCutoff.IsZero() {
		controlCutoff = controlCutoff.Add(minimumAutomaticControlWindow)
	}
	remaining := p.fullDeliveryKWh
	at := p.slots[start].Time
	revenue := decimal.Zero
	for i := start; i < len(p.slots) && remaining > plannerEnergyEpsilon; i++ {
		slot := p.slots[i]
		if !slot.Time.Equal(at) || overlapsRetired(slot.Time, slot.Time.Add(15*time.Minute), p.cfg.RetiredDischargeWindows) {
			return TimeWindow{}, decimal.Zero, false
		}
		capacity := p.dischargePowerKW * 0.25
		used := math.Min(remaining, capacity)
		end := slot.Time.Add(time.Duration(math.Round(used / p.dischargePowerKW * float64(time.Hour))))
		if overlapsRetired(slot.Time, end, p.cfg.RetiredDischargeWindows) {
			return TimeWindow{}, decimal.Zero, false
		}
		revenue = revenue.Add(slot.Export.Mul(decimal.NewFromFloat(used)))
		remaining -= used
		at = slot.Time.Add(15 * time.Minute)
		if remaining <= plannerEnergyEpsilon {
			if !controlCutoff.IsZero() && !end.After(controlCutoff) {
				return TimeWindow{}, decimal.Zero, false
			}
			return TimeWindow{
				Start: p.slots[start].Time,
				End:   end,
				Price: weightedPrice(revenue, p.fullDeliveryKWh),
			}, revenue, true
		}
	}
	return TimeWindow{}, decimal.Zero, false
}

func inventorySaleCandidates(slots []priceSlot, cfg AnalyzerConfig, deliveryKWh, dischargePowerKW float64) []inventorySaleCandidate {
	if cfg.Now.IsZero() || deliveryKWh <= plannerEnergyEpsilon || dischargePowerKW <= 0 {
		return nil
	}
	maxDuration := time.Duration(math.Round(deliveryKWh / dischargePowerKW * float64(time.Hour)))
	var candidates []inventorySaleCandidate
	appendFrom := func(first int, start time.Time) {
		revenue := decimal.Zero
		at := start
		for i := first; i < len(slots); i++ {
			slot := slots[i]
			if (i != first && !slot.Time.Equal(at)) || !slot.Export.IsPositive() {
				break
			}
			end := slot.Time.Add(15 * time.Minute)
			limit := start.Add(maxDuration)
			if end.After(limit) {
				end = limit
			}
			// Retirement can cut a tariff slot in two. Keep the executable
			// prefix, rather than discarding it with the blocked remainder.
			for _, retired := range cfg.RetiredDischargeWindows {
				if retired.Start.Before(end) && retired.End.After(at) {
					end = retired.Start
				}
			}
			if !at.Before(end) {
				break
			}
			energy := dischargePowerKW * end.Sub(at).Hours()
			revenue = revenue.Add(slot.Export.Mul(decimal.NewFromFloat(energy)))
			if end.Sub(start) > minimumAutomaticControlWindow {
				candidates = append(candidates, inventorySaleCandidate{
					window:  TimeWindow{Start: start, End: end, Price: weightedPrice(revenue, dischargePowerKW*end.Sub(start).Hours())},
					revenue: revenue,
					energy:  dischargePowerKW * end.Sub(start).Hours(),
				})
			}
			if !end.Equal(slot.Time.Add(15 * time.Minute)) {
				break
			}
			at = end
		}
	}
	for first, slot := range slots {
		start := slot.Time
		if start.Before(cfg.Now) {
			start = cfg.Now
		}
		slotEnd := slot.Time.Add(15 * time.Minute)
		if !start.Before(slotEnd) {
			continue
		}
		appendFrom(first, start)
		// A completed/retired interval also makes its interior end a legal
		// sale start; waiting for the next tariff boundary can lose value.
		for _, retired := range cfg.RetiredDischargeWindows {
			if retired.End.After(start) && retired.End.Before(slotEnd) {
				appendFrom(first, retired.End)
			}
		}
	}
	return candidates
}

type inventorySaleCandidate struct {
	window  TimeWindow
	revenue decimal.Decimal
	energy  float64
}

func betterValuePlan(candidate, current valuePlan) bool {
	if candidate.value.GreaterThan(current.value) {
		return true
	}
	if !candidate.value.Equal(current.value) {
		return false
	}
	if candidate.sessions != current.sessions {
		return candidate.sessions < current.sessions
	}
	return planStartsEarlier(candidate.cycles, current.cycles)
}

func planStartsEarlier(candidate, current []TradeCycle) bool {
	if len(candidate) == 0 {
		return false
	}
	if len(current) == 0 {
		return true
	}
	return candidate[0].ChargeWindow.Start.Before(current[0].ChargeWindow.Start)
}

func controlSessionCount(windows []TimeWindow) int {
	if len(windows) == 0 {
		return 0
	}
	ordered := append([]TimeWindow(nil), windows...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Start.Before(ordered[j].Start) })
	sessions := 1
	end := ordered[0].End
	for _, window := range ordered[1:] {
		if window.Start.After(end) {
			sessions++
		}
		if window.End.After(end) {
			end = window.End
		}
	}
	return sessions
}

func chargePriceEligible(price, maxPrice decimal.Decimal, minimumSpread float64) bool {
	return !price.GreaterThan(maxPrice) && (minimumSpread > 0 || price.LessThan(maxPrice))
}

func weightedPrice(value decimal.Decimal, energy float64) decimal.Decimal {
	if energy <= plannerEnergyEpsilon {
		return decimal.Zero
	}
	return value.Div(decimal.NewFromFloat(energy))
}

func overlapsRetired(start, end time.Time, retired []TimeWindow) bool {
	for _, window := range retired {
		if start.Before(window.End) && window.Start.Before(end) {
			return true
		}
	}
	return false
}

// inventoryValueBound is a continuous charge-cost relaxation. For a price q,
// cost >= q*demand - sum(max(q-price, 0)*capacity). Its affine upper bound
// safely rejects an entire sale segment before enumerating allocator events.
type inventoryValueBound struct{ price, constant, slope decimal.Decimal }

func (p *gridPlanner) inventoryDual(slot priceSlot, left time.Time, index, cycles, priceIndex int) inventoryValueBound {
	c := p.dischargeCandidates[index]
	chargeKW := decimal.NewFromFloat(p.chargePowerKW)
	saleKW := decimal.NewFromFloat(p.dischargePowerKW)
	demandKW := decimal.NewFromFloat(p.dischargePowerKW / p.roundTrip)
	price := c.eligibleChargeSlots[priceIndex].Value
	constant := c.revenue.Add(p.best(c.window.End, 0, cycles-1).value)
	slope := saleKW.Mul(slot.Export).Sub(price.Mul(demandKW))
	for _, charge := range c.eligibleChargeSlots {
		begin, end := charge.Time, charge.Time.Add(15*time.Minute)
		if begin.Before(left) {
			begin = left
		}
		if !end.After(begin) || !charge.Value.LessThan(price) {
			continue
		}
		discount := price.Sub(charge.Value)
		constant = constant.Add(discount.Mul(decimal.NewFromFloat(p.chargePowerKW * end.Sub(begin).Hours())))
		if charge.Time.Equal(slot.Time) {
			slope = slope.Sub(chargeKW.Mul(discount))
		}
	}
	// The executable allocator may accept this physical energy shortfall.
	constant = constant.Add(price.Abs().Add(slot.Export.Abs()).Mul(decimal.NewFromFloat(plannerEnergyEpsilon)))
	return inventoryValueBound{price: price, constant: constant, slope: slope}
}

// bestWithInventory compares retained inventory with selling it before a grid
// refill. Endpoints include tariff/exhaustion boundaries and every change in
// the executable allocator, not a time grid or a monetary search tolerance.
func (p *gridPlanner) bestWithInventory(initialDC float64, cycles int) (valuePlan, *TimeWindow) {
	best := p.best(p.planningStart, initialDC, cycles)
	var selected *TimeWindow
	// Historical or unobserved SOC cannot authorize a sale of stored energy.
	if !p.cfg.InitialSOCKnown || p.cfg.Now.IsZero() || initialDC <= plannerEnergyEpsilon {
		return best, nil
	}
	scratch := make([]TimeWindow, 0, len(p.slots))
	ed := p.roundTrip / p.chargeEfficiency
	candidates := inventorySaleCandidates(p.slots, p.cfg, initialDC*ed, p.dischargePowerKW)
	type dualKey struct {
		time       int64
		index      int
		priceIndex int
	}
	duals := make(map[dualKey]inventoryValueBound)
	consider := func(sale inventorySaleCandidate, suffix valuePlan) {
		option := valuePlan{cycles: suffix.cycles, value: sale.revenue.Add(suffix.value), sessions: 1 + suffix.sessions}
		if betterInventoryAlternative(option, sale.window, best, selected) {
			best = option
			w := sale.window
			selected = &w
		}
	}
	for _, sale := range candidates {
		consider(sale, p.best(sale.window.End, math.Max(0, initialDC-sale.energy/ed), cycles))
		idx := sort.Search(len(p.slots), func(i int) bool { return !p.slots[i].Time.Before(sale.window.End) }) - 1
		slot := p.slots[idx]
		a := slot.Time
		if a.Before(sale.window.Start) {
			a = sale.window.Start
		}
		bounds := []time.Time{a}
		for _, d := range []time.Duration{minimumAutomaticControlWindow + time.Second, minimumAutomaticControlWindow} {
			cutoff := slot.Time.Add(15*time.Minute - d)
			if cutoff.After(a) && cutoff.Before(sale.window.End) {
				bounds = append(bounds, cutoff)
			}
		}
		bounds = append(bounds, sale.window.End)
		for index, c := range p.dischargeCandidates {
			if !c.feasible || !c.window.Start.After(a) {
				continue
			}
			seen := make(map[int64]bool)
			evaluate := func(at time.Time) {
				if !at.After(a) || !at.Before(sale.window.End) || at.Sub(sale.window.Start) <= minimumAutomaticControlWindow || seen[at.UnixNano()] {
					return
				}
				seen[at.UnixNano()] = true
				energy := p.dischargePowerKW * at.Sub(sale.window.Start).Hours()
				remainingDC := math.Max(0, initialDC-energy/ed)
				requiredDC := p.usableDCKWh - remainingDC
				if requiredDC <= plannerEnergyEpsilon {
					return
				}
				allocation := p.allocateChargeSlices(at, index, requiredDC/p.chargeEfficiency, scratch)
				suffix := p.cycleFromAllocation(index, allocation, cycles)
				if len(suffix.cycles) == 0 {
					return
				}
				revenue := sale.revenue.Sub(slot.Export.Mul(decimal.NewFromFloat(p.dischargePowerKW * sale.window.End.Sub(at).Hours())))
				consider(inventorySaleCandidate{window: TimeWindow{Start: sale.window.Start, End: at, Price: weightedPrice(revenue, energy)}, energy: energy, revenue: revenue}, suffix)
			}
			left, right := a, sale.window.End
			demand := (p.usableDCKWh-initialDC)/p.chargeEfficiency + p.dischargePowerKW/p.roundTrip*left.Sub(sale.window.Start).Hours()
			revenue := sale.revenue.Sub(slot.Export.Mul(decimal.NewFromFloat(p.dischargePowerKW * sale.window.End.Sub(left).Hours())))
			bound := decimal.Zero
			bounded := false
			leftCapacity, rightCapacity := 0.0, 0.0
			rightDemand := demand + p.dischargePowerKW/p.roundTrip*right.Sub(left).Hours()
			leftIndex, rightIndex := -1, -1
			for rank, charge := range c.eligibleChargeSlots {
				begin, end := charge.Time, charge.Time.Add(15*time.Minute)
				if begin.Before(left) {
					begin = left
				}
				if !end.After(begin) {
					continue
				}
				leftCapacity += p.chargePowerKW * end.Sub(begin).Hours()
				if begin.Before(right) {
					begin = right
				}
				rightCapacity += p.chargePowerKW * math.Max(0, end.Sub(begin).Hours())
				if leftIndex < 0 && leftCapacity+plannerEnergyEpsilon >= demand {
					leftIndex = rank
				}
				if rightCapacity+plannerEnergyEpsilon >= rightDemand {
					rightIndex = rank
					break
				}
			}
			if leftIndex < 0 {
				continue
			}
			for _, rank := range []int{leftIndex, rightIndex} {
				if rank < 0 || (bounded && rank == leftIndex) {
					continue
				}
				key := dualKey{left.UnixNano(), index, rank}
				tighter, ok := duals[key]
				if !ok {
					tighter = p.inventoryDual(slot, left, index, cycles, rank)
					duals[key] = tighter
				}
				value := revenue.Add(tighter.constant).Sub(tighter.price.Mul(decimal.NewFromFloat(demand)))
				if tighter.slope.IsPositive() {
					value = value.Add(tighter.slope.Mul(decimal.NewFromFloat(right.Sub(left).Hours())))
				}
				if !bounded || value.LessThan(bound) {
					bound = value
				}
				bounded = true
			}
			if bound.LessThan(best.value) {
				continue
			}
			// Between these cutoffs only the current charge slice loses capacity.
			// Prefix saturation, energy epsilon, the 60-second control minimum,
			// the 61-second replacement tail, and its 60-second retained donor
			// are the allocator branch boundaries. Evaluate neighboring nanos
			// too because duration rounding and strict inequalities matter.
			for region := range len(bounds) - 1 {
				left, right := bounds[region], bounds[region+1]
				demand := (p.usableDCKWh-initialDC)/p.chargeEfficiency + p.dischargePowerKW/p.roundTrip*left.Sub(sale.window.Start).Hours()
				minimum := sale.window.Start.Add(minimumAutomaticControlWindow + time.Nanosecond)
				if !minimum.Before(left) && minimum.Before(right) {
					evaluate(minimum)
				}
				evaluate(left.Add(-time.Nanosecond))
				evaluate(left)
				evaluate(left.Add(time.Nanosecond))
				evaluate(right.Add(-time.Nanosecond))
				evaluate(right)
				evaluate(right.Add(time.Nanosecond))
				capacity := 0.0
				slope := p.dischargePowerKW / p.roundTrip
				generate := func() {
					for _, shift := range []float64{0, plannerEnergyEpsilon, p.chargePowerKW * minimumAutomaticControlWindow.Hours(), p.chargePowerKW * (minimumAutomaticControlWindow + time.Second).Hours(), p.chargePowerKW * (2*minimumAutomaticControlWindow + time.Second).Hours()} {
						hours := (capacity + shift - demand) / slope
						if hours < 0 || hours > right.Sub(left).Hours() {
							continue
						}
						at := left.Add(time.Duration(math.Round(hours * float64(time.Hour))))
						evaluate(at.Add(-time.Nanosecond))
						evaluate(at)
						evaluate(at.Add(time.Nanosecond))
					}
				}
				generate()
				for _, charge := range c.eligibleChargeSlots {
					begin, end := charge.Time, charge.Time.Add(15*time.Minute)
					if begin.Before(left) {
						begin = left
					}
					if end.Sub(begin) <= minimumAutomaticControlWindow {
						continue
					}
					capacity += p.chargePowerKW * end.Sub(begin).Hours()
					if charge.Time.Equal(slot.Time) {
						slope += p.chargePowerKW
					}
					generate()
					if capacity-demand > slope*right.Sub(left).Hours() {
						break
					}
				}
			}
		}
	}
	return best, selected
}
