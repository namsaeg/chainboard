package state

// Refunds. A campaign or bounty that ends returns its coins to the backers
// in proportion to what each put in. Many backers, or many endings at the
// same height, would make one block heavy, so refunds wait in a queue and
// at most RefundsPerBlock backers are paid per block; the coins stay in
// the escrow (and in the supply count) until paid. (A variable so tests can
// lower it; consensus requires every node to use 4096.)
var RefundsPerBlock = 4096

// startRefund queues a campaign or bounty for refunding what its escrow
// holds now.
func (s *State) startRefund(id Hash) {
	if c, ok := s.campaign(id); ok {
		c.Status, c.RefundPot, c.RefundAt = FundRefunding, c.Escrow, 0
		s.Funds.Set(id, c)
	} else if b, ok := s.bounty(id); ok {
		b.Status, b.RefundPot, b.RefundAt = BountyRefunding, b.Escrow, 0
		s.Bounties.Set(id, b)
	} else {
		return
	}
	s.Refunding = append(s.Refunding[:len(s.Refunding):len(s.Refunding)], id)
}

// runRefunds pays the queue's next backers.
func (s *State) runRefunds() {
	budget := RefundsPerBlock
	for budget > 0 && len(s.Refunding) > 0 {
		id := s.Refunding[0]
		var esc, pot, raised *uint64
		var at *int
		var ps interface {
			Len() int
			At(int) PledgeRec
		}
		var save, drop func()
		if c, ok := s.campaign(id); ok {
			esc, pot, raised, at, ps = &c.Escrow, &c.RefundPot, &c.Raised, &c.RefundAt, &c.Pledges
			save, drop = func() { s.Funds.Set(id, c) }, func() { s.Funds.Delete(id) }
		} else if b, ok := s.bounty(id); ok {
			esc, pot, raised, at, ps = &b.Escrow, &b.RefundPot, &b.Raised, &b.RefundAt, &b.Pledges
			save, drop = func() { s.Bounties.Set(id, b) }, func() { s.Bounties.Delete(id) }
		} else {
			s.Refunding = s.Refunding[1:]
			continue
		}
		for budget > 0 && *at < ps.Len() {
			p := ps.At(*at)
			if share := min(mulDiv(p.Amount, *pot, *raised), *esc); share > 0 {
				s.Account(p.From).Balance += share
				*esc -= share
			}
			*at++
			budget--
		}
		if *at < ps.Len() {
			save()
			return
		}
		s.RepairPool += *esc // rounding remainder
		*esc = 0
		drop()
		s.Refunding = s.Refunding[1:]
	}
}

// NewAccountFee is what creating an account costs: its state bytes at the
// permanent byte price, so transfers and tips cannot fill the state for
// nothing.
func (s *State) NewAccountFee() uint64 { return AccountBytes * s.PermByteFee() }

// AccountBytes is roughly what an account takes in the state.
const AccountBytes = 128
