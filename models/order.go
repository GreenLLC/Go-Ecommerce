package models

import (
	"encoding/json"
	"time"

	"github.com/netlify/gocommerce/calculator"
	"github.com/pborman/uuid"
)

// PendingState is the pending state of an Order
const PendingState = "pending"

// PaidState is the paid state of an Order
const PaidState = "paid"

// ShippedState is the shipped state of an Order
const ShippedState = "shipped"

// FailedState is the failed state of an Order
const FailedState = "failed"

// NumberType | StringType | BoolType are the different types supported in custom data for orders
const (
	NumberType = iota
	StringType
	BoolType
)

// Order model
type Order struct {
	ID string `json:"id"`

	IP string `json:"ip"`

	User      *User  `json:"user,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	SessionID string `json:"-"`

	Email string `json:"email"`

	LineItems []*LineItem `json:"line_items"`

	Downloads []Download `json:"downloads"`

	Currency string `json:"currency"`
	Taxes    uint64 `json:"taxes"`
	Shipping uint64 `json:"shipping"`
	SubTotal uint64 `json:"subtotal"`
	Discount uint64 `json:"discount"`

	Total uint64 `json:"total"`

	PaymentState     string `json:"payment_state"`
	FulfillmentState string `json:"fulfillment_state"`
	State            string `json:"state"`

	PaymentProcessor string `json:"payment_processor"`

	Transactions []*Transaction `json:"transactions"`
	Notes        []*OrderNote   `json:"notes"`

	ShippingAddress   Address `json:"shipping_address" gorm:"ForeignKey:ShippingAddressID"`
	ShippingAddressID string  `json:"shipping_address_id"`

	BillingAddress   Address `json:"billing_address" gorm:"ForeignKey:BillingAddressID"`
	BillingAddressID string  `json:"billing_address_id"`

	VATNumber string `json:"vatnumber"`

	MetaData    map[string]interface{} `sql:"-" json:"meta"`
	RawMetaData string                 `json:"-"`

	CouponCode string `json:"coupon_code,omitempty"`

	Coupon    *Coupon `json:"coupon,omitempty" sql:"-"`
	RawCoupon string  `json:"-"`

	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	DeletedAt *time.Time `json:"-" sql:"index:idx_orders_deleted_at"`
}

// TableName returns the database table name for the Order model.
func (Order) TableName() string {
	return tableName("orders")
}

// AfterFind database callback.
func (o *Order) AfterFind() error {
	if o.RawMetaData != "" {
		err := json.Unmarshal([]byte(o.RawMetaData), &o.MetaData)
		if err != nil {
			return err
		}
	}
	if o.RawCoupon != "" {
		o.Coupon = &Coupon{}
		err := json.Unmarshal([]byte(o.RawCoupon), &o.Coupon)
		if err != nil {
			return err
		}
	}

	return nil
}

// BeforeUpdate database callback.
func (o *Order) BeforeUpdate() error {
	if o.MetaData != nil {
		data, err := json.Marshal(o.MetaData)
		if err != nil {
			return err
		}
		o.RawMetaData = string(data)
	}
	if o.Coupon != nil {
		data, err := json.Marshal(o.Coupon)
		if err != nil {
			return err
		}
		o.RawCoupon = string(data)
	}

	return nil
}

// NewOrder creates a new pending Order.
func NewOrder(sessionID, email, currency string) *Order {
	order := &Order{
		ID:        uuid.NewRandom().String(),
		SessionID: sessionID,
		Email:     email,
		Currency:  currency,
	}
	order.PaymentState = PendingState
	order.FulfillmentState = PendingState
	order.State = PendingState
	return order
}

// CalculateTotal calculates the total price of an Order.
func (o *Order) CalculateTotal(settings *calculator.Settings, claims map[string]interface{}) {
	items := make([]calculator.Item, len(o.LineItems))
	for i, item := range o.LineItems {
		items[i] = item
	}

	price := calculator.CalculatePrice(settings, claims, o.ShippingAddress.Country, o.Currency, o.Coupon, items)

	o.SubTotal = price.Subtotal
	o.Taxes = price.Taxes
	o.Discount = price.Discount
	o.Total = price.Total
}
