package store

import (
	"errors"
	"fmt"
	"testing"
)

// The classic problem, built out of nothing but declarations: an order, a
// payment against it, and the double-entry lines that record the money.
//
// Nobody opens a transaction. The whole flow is one declared operation, which
// is what makes its cost knowable and what stops a client holding the single
// writer open while it thinks.
//
// Two things are worth watching for in what follows, because they are the
// bugs this domain actually has:
//
//   - The payment must not be taken twice, and must not be taken against an
//     order that is no longer waiting for it. That is the condition on the
//     step, checked against the document as it stands.
//   - The ledger must balance. Not checked afterwards — the operation writes
//     one debit and one credit from a single amount, so it cannot write a
//     posting that does not balance. A rule enforced by the shape of the
//     declaration needs nobody to remember it.
func ledger(t *testing.T, store *Store) {
	t.Helper()

	for _, spec := range []Spec{
		{
			Name: "orders",
			Key:  Key{Path: "id", Type: TypeString},
			Indexes: []Index{{
				Name:   "by_status",
				Fields: []Field{{Path: "status", Type: TypeString, Missing: MissingSkip}},
			}},
		},
		{
			Name: "payments",
			Key:  Key{Path: "id", Type: TypeString, Auto: "ulid"},
			Indexes: []Index{{
				Name:   "by_order",
				Fields: []Field{{Path: "order", Type: TypeString, Missing: MissingSkip}},
			}},
		},
		{
			Name: "entries",
			Key:  Key{Path: "id", Type: TypeString, Auto: "ulid"},
			Indexes: []Index{{
				Name: "by_account",
				Fields: []Field{
					{Path: "account", Type: TypeString, Missing: MissingSkip},
					{Path: "at", Type: TypeNumber, Missing: MissingLast},
				},
			}},
		},
	} {
		if _, err := store.Declare(spec); err != nil {
			t.Fatalf("declare %q: %v", spec.Name, err)
		}
	}

	yes := true

	for _, operation := range []Operation{
		{
			Name: "orders.place", Collection: "orders", Action: ActionInsert,
			Input: []Parameter{
				{Name: "id", Type: TypeString, Required: true},
				{Name: "customer", Type: TypeString, Required: true},
				{Name: "total", Type: TypeNumber, Required: true},
			},
			Document: map[string]Term{
				"id": {Arg: "id"}, "customer": {Arg: "customer"},
				"total": {Arg: "total"}, "status": {Value: "awaiting_payment"},
			},
		},
		{
			// The whole flow. Five steps, one transaction, no begin.
			Name: "orders.pay", Collection: "orders", Action: ActionBatch,
			Input: []Parameter{
				{Name: "order", Type: TypeString, Required: true},
				{Name: "amount", Type: TypeNumber, Required: true},
				{Name: "at", Type: TypeNumber, Required: true},
				{Name: "reference", Type: TypeString, Required: true},
			},
			Steps: []Step{
				{
					// Only against an order that is still waiting. A payment
					// taken twice is the bug this exists to make impossible.
					Name: "order", Action: ActionUpdate, Collection: "orders",
					Key:    &Term{Arg: "order"},
					Exists: &yes,
					Require: []Condition{
						{Path: "status", Equals: &Term{Value: "awaiting_payment"}},
						{Path: "total", Equals: &Term{Arg: "amount"}},
					},
					Set: map[string]Term{"status": {Value: "paid"}, "paid_at": {Arg: "at"}},
				},
				{
					Name: "payment", Action: ActionInsert, Collection: "payments",
					Document: map[string]Term{
						"order":  {Step: "order", Field: "key"},
						"amount": {Arg: "amount"}, "at": {Arg: "at"},
						"reference": {Arg: "reference"},
					},
				},
				{
					// Debit what came in, credit what was earned. One amount,
					// two lines, opposite signs: a posting that does not
					// balance is not expressible.
					Action: ActionInsert, Collection: "entries",
					Document: map[string]Term{
						"account": {Value: "assets:cash"}, "side": {Value: "debit"},
						"amount": {Arg: "amount"}, "at": {Arg: "at"},
						"payment": {Step: "payment", Field: "key"},
					},
				},
				{
					Action: ActionInsert, Collection: "entries",
					Document: map[string]Term{
						"account": {Value: "income:sales"}, "side": {Value: "credit"},
						"amount": {Arg: "amount"}, "at": {Arg: "at"},
						"payment": {Step: "payment", Field: "key"},
					},
				},
			},
		},
	} {
		if _, err := store.DeclareOperation(operation); err != nil {
			t.Fatalf("declare %q: %v", operation.Name, err)
		}
	}

	// Declaring is not committed one at a time — `rsql apply` declares a whole
	// file and commits once, so that half a schema never lands — which means
	// the setup has to say when it is finished.
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}
}

// balance adds up a ledger the way an accountant would: debits positive,
// credits negative, and the answer must be zero.
func balance(t *testing.T, store *Store) (total float64, lines int) {
	t.Helper()

	entries, err := store.Collection("entries")
	if err != nil {
		t.Fatal(err)
	}
	if err := entries.Walk(func(_ any, entry map[string]any) bool {
		amount, _ := entry["amount"].(float64)
		if entry["side"] == "credit" {
			amount = -amount
		}
		total += amount
		lines++
		return true
	}); err != nil {
		t.Fatal(err)
	}
	return total, lines
}

func TestAnOrderAPaymentAndALedgerInOneTransaction(t *testing.T) {
	_, store := fresh(t, 80)
	ledger(t, store)

	if _, err := store.Invoke(Caller{Actor: "shop"}, "orders.place", 0, map[string]any{
		"id": "ord-1", "customer": "ann", "total": 99.5,
	}); err != nil {
		t.Fatal(err)
	}

	result, err := store.Invoke(Caller{Actor: "shop"}, "orders.pay", 0, map[string]any{
		"order": "ord-1", "amount": 99.5, "at": 1700000000000.0, "reference": "card-abc",
	})
	if err != nil {
		t.Fatalf("paying: %v", err)
	}
	if result.Changed != 4 {
		t.Errorf("the batch changed %d things, want 4", result.Changed)
	}

	orders, _ := store.Collection("orders")
	order, found, err := orders.Get("ord-1")
	if err != nil || !found {
		t.Fatalf("the order: %v, %v", found, err)
	}
	if order["status"] != "paid" {
		t.Errorf("the order is %v", order["status"])
	}

	payments, _ := store.Collection("payments")
	against := scan(t, payments, "by_order", Range{
		From: &Bound{Values: []any{"ord-1"}}, To: &Bound{Values: []any{"ord-1"}},
	})
	if len(against) != 1 {
		t.Fatalf("%d payments against the order", len(against))
	}

	total, lines := balance(t, store)
	if lines != 2 {
		t.Errorf("%d ledger lines, want 2", lines)
	}
	if total != 0 {
		t.Errorf("the ledger is out by %v", total)
	}

	// And the audit trail says who and under which operation, because it is
	// the same log everything else uses.
	var posted int
	if err := store.Changes(1, func(change Change) bool {
		if change.Collection == "entries" && change.By.Operation == "orders.pay" && change.By.Actor == "shop" {
			posted++
		}
		return true
	}); err != nil {
		t.Fatal(err)
	}
	if posted != 2 {
		t.Errorf("%d ledger lines are attributed to the operation that made them", posted)
	}
}

// The bug this domain actually has: the same payment arriving twice. Once
// because a customer double-clicked, once because a connection dropped and
// the client retried.
func TestAPaymentCannotBeTakenTwice(t *testing.T) {
	_, store := fresh(t, 81)
	ledger(t, store)

	if _, err := store.Invoke(Caller{}, "orders.place", 0, map[string]any{
		"id": "ord-2", "customer": "bob", "total": 40.0,
	}); err != nil {
		t.Fatal(err)
	}

	arguments := map[string]any{
		"order": "ord-2", "amount": 40.0, "at": 1700000000000.0, "reference": "card-xyz",
	}
	if _, err := store.Invoke(Caller{}, "orders.pay", 0, arguments); err != nil {
		t.Fatal(err)
	}

	// The second time the order is no longer awaiting payment, so the
	// condition refuses it — and nothing at all is written.
	_, err := store.Invoke(Caller{}, "orders.pay", 0, arguments)
	if !errors.Is(err, ErrCondition) {
		t.Fatalf("the second payment: want ErrCondition, got %v", err)
	}

	total, lines := balance(t, store)
	if lines != 2 {
		t.Errorf("%d ledger lines after a refused second payment, want 2", lines)
	}
	if total != 0 {
		t.Errorf("the ledger is out by %v", total)
	}

	payments, _ := store.Collection("payments")
	count := 0
	if err := payments.Walk(func(any, map[string]any) bool { count++; return true }); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("%d payments were taken, want 1", count)
	}

	// The same call under one write id is answered rather than refused, which
	// is what makes a retry after a dropped connection safe.
	third := Caller{WriteID: "01RETRY0000000000000000000"}
	if _, err := store.Invoke(third, "orders.pay", 0, map[string]any{
		"order": "ord-2", "amount": 40.0, "at": 1700000000001.0, "reference": "card-xyz",
	}); !errors.Is(err, ErrCondition) {
		t.Fatalf("a retry of a payment already taken: %v", err)
	}
}

// A step that fails after earlier steps have written must leave none of them.
// A payment recorded without its ledger lines is exactly the state an
// accountant cannot reconcile and nobody notices for a month.
func TestAFailedStepLeavesNoneOfTheEarlierOnes(t *testing.T) {
	_, store := fresh(t, 82)
	ledger(t, store)

	// An operation whose last step cannot work: it writes a payment, then
	// tries to insert a ledger line under a key that is already taken.
	yes := true
	if _, err := store.DeclareOperation(Operation{
		Name: "orders.pay_broken", Collection: "orders", Action: ActionBatch,
		Input: []Parameter{
			{Name: "order", Type: TypeString, Required: true},
			{Name: "amount", Type: TypeNumber, Required: true},
			{Name: "entry", Type: TypeString, Required: true},
		},
		Steps: []Step{
			{
				Name: "order", Action: ActionUpdate, Collection: "orders",
				Key: &Term{Arg: "order"}, Exists: &yes,
				Require: []Condition{{Path: "status", Equals: &Term{Value: "awaiting_payment"}}},
				Set:     map[string]Term{"status": {Value: "paid"}},
			},
			{
				Name: "payment", Action: ActionInsert, Collection: "payments",
				Document: map[string]Term{"order": {Step: "order", Field: "key"}, "amount": {Arg: "amount"}},
			},
			{
				// Insert, so a key already there is refused.
				Action: ActionInsert, Collection: "entries",
				Document: map[string]Term{
					"id": {Arg: "entry"}, "account": {Value: "assets:cash"},
					"side": {Value: "debit"}, "amount": {Arg: "amount"},
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Invoke(Caller{}, "orders.place", 0, map[string]any{
		"id": "ord-3", "customer": "cat", "total": 10.0,
	}); err != nil {
		t.Fatal(err)
	}
	// The key the last step will collide with.
	entries, _ := store.Collection("entries")
	if _, err := entries.Put(map[string]any{
		"id": "taken", "account": "assets:cash", "side": "debit", "amount": 0.0,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	before, _ := balance(t, store)

	_, err := store.Invoke(Caller{}, "orders.pay_broken", 0, map[string]any{
		"order": "ord-3", "amount": 10.0, "entry": "taken",
	})
	if !errors.Is(err, ErrExists) {
		t.Fatalf("want ErrExists, got %v", err)
	}

	// None of it happened: not the order's new status, not the payment.
	orders, err := store.Collection("orders")
	if err != nil {
		t.Fatal(err)
	}
	order, _, err := orders.Get("ord-3")
	if err != nil {
		t.Fatal(err)
	}
	if order["status"] != "awaiting_payment" {
		t.Errorf("the order was left %v by a transaction that failed", order["status"])
	}

	payments, err := store.Collection("payments")
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	if err := payments.Walk(func(any, map[string]any) bool { count++; return true }); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Errorf("%d payments survived a transaction that failed", count)
	}

	after, _ := balance(t, store)
	if after != before {
		t.Errorf("the ledger moved from %v to %v through a transaction that failed", before, after)
	}
}

// Many orders paid one after another. The ledger balances after every one of
// them, which is the only statement an accountant cares about.
func TestTheLedgerBalancesThroughALotOfWork(t *testing.T) {
	_, store := fresh(t, 83)
	ledger(t, store)

	const orders = 200
	for i := 0; i < orders; i++ {
		amount := float64(i%97) + 0.5
		id := fmt.Sprintf("ord-%03d", i)

		if _, err := store.Invoke(Caller{}, "orders.place", 0, map[string]any{
			"id": id, "customer": fmt.Sprintf("c%d", i%7), "total": amount,
		}); err != nil {
			t.Fatalf("placing %s: %v", id, err)
		}
		if _, err := store.Invoke(Caller{Actor: "shop"}, "orders.pay", 0, map[string]any{
			"order": id, "amount": amount, "at": float64(1700000000000 + i), "reference": fmt.Sprintf("r%d", i),
		}); err != nil {
			t.Fatalf("paying %s: %v", id, err)
		}

		if total, _ := balance(t, store); total != 0 {
			t.Fatalf("after %s the ledger is out by %v", id, total)
		}
	}

	total, lines := balance(t, store)
	if lines != orders*2 {
		t.Errorf("%d ledger lines, want %d", lines, orders*2)
	}
	if total != 0 {
		t.Errorf("the ledger is out by %v", total)
	}

	// An amount that does not match what the order says is refused, so a
	// payment for the wrong money never reaches the ledger.
	if _, err := store.Invoke(Caller{}, "orders.place", 0, map[string]any{
		"id": "ord-wrong", "customer": "ann", "total": 50.0,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Invoke(Caller{}, "orders.pay", 0, map[string]any{
		"order": "ord-wrong", "amount": 5.0, "at": 1700000000000.0, "reference": "short",
	}); !errors.Is(err, ErrCondition) {
		t.Errorf("paying the wrong amount: want ErrCondition, got %v", err)
	}
	if total, _ := balance(t, store); total != 0 {
		t.Errorf("a refused payment moved the ledger by %v", total)
	}
}

// Everything a step can insist on, and what happens when it is not so. These
// are the checks that turn "the order was still pending when I looked" from a
// hope into something the store enforces, so each of them has to actually
// refuse.
func TestWhatAStepInsistsOn(t *testing.T) {
	_, store := fresh(t, 84)
	ledger(t, store)

	yes, no := true, false
	if _, err := store.DeclareOperation(Operation{
		Name: "orders.checks", Collection: "orders", Action: ActionBatch,
		Input: []Parameter{
			{Name: "order", Type: TypeString, Required: true},
			{Name: "expect", Type: TypeString},
			{Name: "amount", Type: TypeNumber},
		},
		Steps: []Step{{
			Action: ActionUpdate, Collection: "orders",
			Key: &Term{Arg: "order"}, Exists: &yes,
			Require: []Condition{{Path: "status", Equals: &Term{Arg: "expect"}}},
			Set:     map[string]Term{"seen": {Value: true}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeclareOperation(Operation{
		Name: "orders.must_be_new", Collection: "orders", Action: ActionBatch,
		Input: []Parameter{{Name: "order", Type: TypeString, Required: true}},
		Steps: []Step{{Action: ActionUpdate, Collection: "orders", Key: &Term{Arg: "order"}, Exists: &no, Set: map[string]Term{"seen": {Value: true}}}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeclareOperation(Operation{
		Name: "orders.unpaid", Collection: "orders", Action: ActionBatch,
		Input: []Parameter{{Name: "order", Type: TypeString, Required: true}},
		Steps: []Step{{
			Action: ActionUpdate, Collection: "orders", Key: &Term{Arg: "order"},
			Require: []Condition{{Path: "paid_at", Absent: true}},
			Set:     map[string]Term{"seen": {Value: true}},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Invoke(Caller{}, "orders.place", 0, map[string]any{
		"id": "ord-9", "customer": "ann", "total": 12.0,
	}); err != nil {
		t.Fatal(err)
	}

	// A document that is not there cannot satisfy anything.
	if _, err := store.Invoke(Caller{}, "orders.checks", 0, map[string]any{
		"order": "nobody", "expect": "awaiting_payment",
	}); !errors.Is(err, ErrMissing) {
		t.Errorf("a condition on a document that is not there: want ErrMissing, got %v", err)
	}

	// A field the document does not have is not equal to anything.
	if _, err := store.Invoke(Caller{}, "orders.checks", 0, map[string]any{
		"order": "ord-9", "expect": "awaiting_payment",
	}); err != nil {
		t.Fatalf("the condition that holds: %v", err)
	}
	if _, err := store.Invoke(Caller{}, "orders.checks", 0, map[string]any{
		"order": "ord-9", "expect": "shipped",
	}); !errors.Is(err, ErrCondition) {
		t.Errorf("a condition that does not hold: want ErrCondition, got %v", err)
	}

	// "must not exist" refuses a document that does.
	if _, err := store.Invoke(Caller{}, "orders.must_be_new", 0, map[string]any{
		"order": "ord-9",
	}); !errors.Is(err, ErrExists) {
		t.Errorf("a step that wants nothing there: want ErrExists, got %v", err)
	}
	// And "must exist" refuses one that does not.
	if _, err := store.Invoke(Caller{}, "orders.checks", 0, map[string]any{
		"order": "not-an-order", "expect": "awaiting_payment",
	}); !errors.Is(err, ErrMissing) {
		t.Errorf("a step that wants something there: want ErrMissing, got %v", err)
	}

	// "must be absent" holds while the field is not there, and refuses once it
	// is — which is how "not yet paid" is said without a status to read.
	if _, err := store.Invoke(Caller{}, "orders.unpaid", 0, map[string]any{"order": "ord-9"}); err != nil {
		t.Errorf("absent while it is absent: %v", err)
	}
	if _, err := store.Invoke(Caller{}, "orders.pay", 0, map[string]any{
		"order": "ord-9", "amount": 12.0, "at": 1700000000000.0, "reference": "r",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Invoke(Caller{}, "orders.unpaid", 0, map[string]any{"order": "ord-9"}); !errors.Is(err, ErrCondition) {
		t.Errorf("absent once it is there: want ErrCondition, got %v", err)
	}

}

// A condition compares numbers as numbers. A value from a document is a
// float64 because it came through JSON; one written into a declaration may be
// an integer. Comparing them as opaque values makes 40 and 40.0 different
// things, which is the sort of difference nobody debugging a refused write
// would think to look for.
func TestANumberEqualsTheSameNumberHoweverItWasWritten(t *testing.T) {
	_, store := fresh(t, 85)
	ledger(t, store)

	if _, err := store.DeclareOperation(Operation{
		Name: "orders.worth", Collection: "orders", Action: ActionBatch,
		Input: []Parameter{{Name: "order", Type: TypeString, Required: true}},
		Steps: []Step{{
			Action: ActionUpdate, Collection: "orders", Key: &Term{Arg: "order"},
			// Written as an integer in the declaration; stored as a float.
			Require: []Condition{{Path: "total", Equals: &Term{Value: 40}}},
			Set:     map[string]Term{"checked": {Value: true}},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Invoke(Caller{}, "orders.place", 0, map[string]any{
		"id": "ord-int", "customer": "ann", "total": 40.0,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Invoke(Caller{}, "orders.worth", 0, map[string]any{"order": "ord-int"}); err != nil {
		t.Errorf("40 written as an integer did not match 40 stored as a number: %v", err)
	}

	// The declared constant above came back through JSON as a float, so that
	// path alone proves less than it looks. An argument from a caller inside
	// this process does not: it arrives as whatever Go type was passed.
	if _, err := store.DeclareOperation(Operation{
		Name: "orders.worth_arg", Collection: "orders", Action: ActionBatch,
		Input: []Parameter{
			{Name: "order", Type: TypeString, Required: true},
			{Name: "amount", Type: TypeNumber, Required: true},
		},
		Steps: []Step{{
			Action: ActionUpdate, Collection: "orders", Key: &Term{Arg: "order"},
			Require: []Condition{{Path: "total", Equals: &Term{Arg: "amount"}}},
			Set:     map[string]Term{"checked": {Value: true}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	// An int against a float64 that came out of a document.
	if _, err := store.Invoke(Caller{}, "orders.worth_arg", 0, map[string]any{
		"order": "ord-int", "amount": 40,
	}); err != nil {
		t.Errorf("an integer argument against a stored number: %v", err)
	}
	// And a number that really is different is still refused.
	if _, err := store.Invoke(Caller{}, "orders.worth_arg", 0, map[string]any{
		"order": "ord-int", "amount": 41,
	}); !errors.Is(err, ErrCondition) {
		t.Errorf("a different number: want ErrCondition, got %v", err)
	}
}

// A batch abandons the transaction when a step fails, and there are no
// savepoints. So it refuses to start on top of work somebody else has not
// committed — otherwise a failure would take that with it, silently, at the
// worst possible moment.
func TestABatchRefusesToStartOnTopOfHalfWrittenWork(t *testing.T) {
	_, store := fresh(t, 86)
	ledger(t, store)

	if _, err := store.Invoke(Caller{}, "orders.place", 0, map[string]any{
		"id": "ord-10", "customer": "ann", "total": 5.0,
	}); err != nil {
		t.Fatal(err)
	}

	// Written straight through the collection, which does not commit.
	orders, err := store.Collection("orders")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := orders.Put(map[string]any{
		"id": "ord-11", "customer": "bob", "total": 6.0, "status": "awaiting_payment",
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Invoke(Caller{}, "orders.pay", 0, map[string]any{
		"order": "ord-10", "amount": 5.0, "at": 1700000000000.0, "reference": "r",
	}); !errors.Is(err, ErrUncommitted) {
		t.Fatalf("want ErrUncommitted, got %v", err)
	}

	// The half-written work is still there, untouched: being refused is not
	// the same as being rolled back.
	if _, found, err := orders.Get("ord-11"); err != nil || !found {
		t.Errorf("the uncommitted work was lost: %v, %v", found, err)
	}

	// Commit it, and the batch runs.
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Invoke(Caller{}, "orders.pay", 0, map[string]any{
		"order": "ord-10", "amount": 5.0, "at": 1700000000000.0, "reference": "r",
	}); err != nil {
		t.Errorf("after committing: %v", err)
	}
}

// A step may only use what an earlier step produced. A reference forward, or
// to itself, is refused where it is written rather than found at call time.
func TestAStepCanOnlyUseWhatCameBeforeIt(t *testing.T) {
	_, store := fresh(t, 87)
	ledger(t, store)

	for name, steps := range map[string][]Step{
		"a step referring to itself": {{
			Name: "one", Action: ActionInsert, Collection: "payments",
			Document: map[string]Term{"order": {Step: "one", Field: "key"}},
		}},
		"a step referring to a later one": {
			{
				Name: "first", Action: ActionInsert, Collection: "payments",
				Document: map[string]Term{"order": {Step: "second", Field: "key"}},
			},
			{
				Name: "second", Action: ActionInsert, Collection: "payments",
				Document: map[string]Term{"order": {Value: "x"}},
			},
		},
		"a step referring to one that does not exist": {{
			Action: ActionInsert, Collection: "payments",
			Document: map[string]Term{"order": {Step: "nowhere", Field: "key"}},
		}},
		"asking a step for something other than its key": {
			{Name: "first", Action: ActionInsert, Collection: "payments", Document: map[string]Term{"order": {Value: "x"}}},
			{Action: ActionInsert, Collection: "payments", Document: map[string]Term{"order": {Step: "first", Field: "amount"}}},
		},
		"two steps of one name": {
			{Name: "same", Action: ActionInsert, Collection: "payments", Document: map[string]Term{"order": {Value: "x"}}},
			{Name: "same", Action: ActionInsert, Collection: "payments", Document: map[string]Term{"order": {Value: "y"}}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := store.DeclareOperation(Operation{
				Name: "broken", Collection: "payments", Action: ActionBatch, Steps: steps,
			}); !errors.Is(err, ErrDeclaration) {
				t.Errorf("want ErrDeclaration, got %v", err)
			}
		})
	}

	// And one that is properly ordered is accepted.
	if _, err := store.DeclareOperation(Operation{
		Name: "fine", Collection: "payments", Action: ActionBatch,
		Input: []Parameter{{Name: "order", Type: TypeString, Required: true}},
		Steps: []Step{
			{Name: "first", Action: ActionInsert, Collection: "payments", Document: map[string]Term{"order": {Arg: "order"}}},
			{Action: ActionInsert, Collection: "payments", Document: map[string]Term{"order": {Step: "first", Field: "key"}}},
		},
	}); err != nil {
		t.Errorf("a properly ordered batch: %v", err)
	}
}

// An insert inside a batch is still an insert: it refuses a key that is taken,
// rather than writing over what is there.
func TestAnInsertInsideABatchWillNotWriteOverADocument(t *testing.T) {
	_, store := fresh(t, 88)
	ledger(t, store)

	if _, err := store.DeclareOperation(Operation{
		Name: "entries.post", Collection: "entries", Action: ActionBatch,
		Input: []Parameter{
			{Name: "id", Type: TypeString, Required: true},
			{Name: "amount", Type: TypeNumber, Required: true},
		},
		Steps: []Step{{
			Action: ActionInsert, Collection: "entries",
			Document: map[string]Term{
				"id": {Arg: "id"}, "account": {Value: "assets:cash"},
				"side": {Value: "debit"}, "amount": {Arg: "amount"},
			},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	// Declaring does not commit itself, and a batch will not start on top of
	// something half-written — so the setup says when it is finished, exactly
	// as applying a schema file does.
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Invoke(Caller{}, "entries.post", 0, map[string]any{"id": "e-1", "amount": 1.0}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Invoke(Caller{}, "entries.post", 0, map[string]any{"id": "e-1", "amount": 999.0}); !errors.Is(err, ErrExists) {
		t.Fatalf("want ErrExists, got %v", err)
	}

	entries, err := store.Collection("entries")
	if err != nil {
		t.Fatal(err)
	}
	entry, _, err := entries.Get("e-1")
	if err != nil {
		t.Fatal(err)
	}
	if entry["amount"] != 1.0 {
		t.Errorf("the entry was written over: %v", entry["amount"])
	}
}

// TestNothingIsTrueOfADocumentThatIsNotThere: a condition says something about
// a document, so a document that is not there does not satisfy it — it is
// missing, which is a different answer and a different repair.
//
// The distinction only shows on a step whose action does not mind an absent
// document. A delete is the honest one: deleting what is not there is not an
// error, so if a condition on nothing quietly passed, `cancel an order that is
// still pending` would report success for an order that was never placed.
func TestNothingIsTrueOfADocumentThatIsNotThere(t *testing.T) {
	_, store := fresh(t, 90)
	ledger(t, store)

	if _, err := store.DeclareOperation(Operation{
		Name: "orders.cancel", Collection: "orders", Action: ActionBatch,
		Input: []Parameter{{Name: "order", Type: TypeString, Required: true}},
		Steps: []Step{{
			Action: ActionDelete, Collection: "orders", Key: &Term{Arg: "order"},
			Require: []Condition{{Path: "status", Equals: &Term{Value: "awaiting_payment"}}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Invoke(Caller{}, "orders.place", 0, map[string]any{
		"id": "ord-cancel", "customer": "cus-1", "total": 10.0,
	}); err != nil {
		t.Fatal(err)
	}

	// An order that is there and still pending goes.
	done, err := store.Invoke(Caller{}, "orders.cancel", 0, map[string]any{"order": "ord-cancel"})
	if err != nil {
		t.Fatal(err)
	}
	if done.Changed != 1 {
		t.Errorf("cancelling a pending order changed %d documents", done.Changed)
	}

	// The same call again, now that it is gone. Missing, not satisfied.
	if _, err := store.Invoke(Caller{}, "orders.cancel", 0, map[string]any{"order": "ord-cancel"}); !errors.Is(err, ErrMissing) {
		t.Errorf("cancelling what is not there: want ErrMissing, got %v", err)
	}
	if _, err := store.Invoke(Caller{}, "orders.cancel", 0, map[string]any{"order": "never-placed"}); !errors.Is(err, ErrMissing) {
		t.Errorf("cancelling what was never placed: want ErrMissing, got %v", err)
	}
}

// TestAFieldSetToNothingIsNotAFieldThatIsNotThere: `absent` asks whether a
// field was written; a condition on a value asks what it is. A document that
// never carried the field answers the first and not the second.
//
// Written as a condition on null because that is the only value where the two
// could be confused: everywhere else an absent field fails the comparison by
// accident. Here it would pass by accident, and an operation declared to run
// only while a field is explicitly cleared would also run on documents that
// never had it.
func TestAFieldSetToNothingIsNotAFieldThatIsNotThere(t *testing.T) {
	_, store := fresh(t, 91)
	ledger(t, store)

	if _, err := store.DeclareOperation(Operation{
		Name: "orders.reship", Collection: "orders", Action: ActionBatch,
		Input: []Parameter{{Name: "order", Type: TypeString, Required: true}},
		Steps: []Step{{
			Action: ActionUpdate, Collection: "orders", Key: &Term{Arg: "order"},
			Require: []Condition{{Path: "courier", Equals: &Term{Constant: true}}},
			Set:     map[string]Term{"reshipped": {Value: true}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DeclareOperation(Operation{
		Name: "orders.clear_courier", Collection: "orders", Action: ActionBatch,
		Input: []Parameter{{Name: "order", Type: TypeString, Required: true}},
		Steps: []Step{{
			Action: ActionUpdate, Collection: "orders", Key: &Term{Arg: "order"},
			Set: map[string]Term{"courier": {Constant: true}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(); err != nil {
		t.Fatal(err)
	}

	if _, err := store.Invoke(Caller{}, "orders.place", 0, map[string]any{
		"id": "ord-ship", "customer": "cus-1", "total": 10.0,
	}); err != nil {
		t.Fatal(err)
	}

	// The order has no courier field at all.
	if _, err := store.Invoke(Caller{}, "orders.reship", 0, map[string]any{"order": "ord-ship"}); !errors.Is(err, ErrCondition) {
		t.Errorf("a field that was never written: want ErrCondition, got %v", err)
	}

	// Now it has one, and it is null.
	if _, err := store.Invoke(Caller{}, "orders.clear_courier", 0, map[string]any{"order": "ord-ship"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Invoke(Caller{}, "orders.reship", 0, map[string]any{"order": "ord-ship"}); err != nil {
		t.Errorf("a field written as null: %v", err)
	}
}
