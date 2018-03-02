package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/Sirupsen/logrus"
	"github.com/guregu/kami"
	"github.com/jinzhu/gorm"
	stripe "github.com/stripe/stripe-go"
	"github.com/stripe/stripe-go/charge"

	"github.com/netlify/netlify-commerce/models"
)

// MaxConcurrentLookups controls the number of simultaneous HTTP Order lookups
const MaxConcurrentLookups = 10

// PaymentParams holds the parameters for creating a payment
type PaymentParams struct {
	Amount      uint64 `json:"amount"`
	Currency    string `json:"currency"`
	StripeToken string `json:"stripe_token"`
}

// PaymentListForUser is the endpoint for listing transactions for a user.
// The ID in the claim and the ID in the path must match (or have admin override)
func (a *API) PaymentListForUser(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	log, claims, userID, httpErr := initEndpoint(ctx, w, "user_id", true)
	if httpErr != nil {
		sendJSON(w, httpErr.Code, httpErr)
		return
	}

	if userID != claims.ID && !isAdmin(ctx) {
		log.Warn("Illegal access attempt")
		unauthorizedError(w, "Can't access payments for this user")
		return
	}

	trans, httpErr := queryForTransactions(a.db, log, "user_id = ?", userID)
	if httpErr != nil {
		sendJSON(w, httpErr.Code, httpErr)
		return
	}
	sendJSON(w, 200, trans)
}

// PaymentListForOrder is the endpoint for listing transactions for an order. You must be the owner
// of the order (user_id) or an admin. Listing the payments for an anon order.
func (a *API) PaymentListForOrder(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	log, claims, orderID, httpErr := initEndpoint(ctx, w, "order_id", true)
	if httpErr != nil {
		sendJSON(w, httpErr.Code, httpErr)
		return
	}

	order, httpErr := queryForOrder(a.db, orderID, log)
	if httpErr != nil {
		sendJSON(w, httpErr.Code, httpErr)
		return
	}

	isAdmin := isAdmin(ctx)

	// now we need to check if you're allowed to look at this order
	if order.UserID == "" && !isAdmin {
		// anon order ~ only accessible by an admin
		log.Warn("Queried for an anonymous order but not as admin")
		sendJSON(w, 401, unauthorizedError(w, "Anonymous orders must be accessed by admins"))
		return
	}

	if order.UserID != claims.ID && !isAdmin {
		log.Warnf("Attempt to access order as %s, but order.UserID is %s", claims.ID, order.UserID)
		sendJSON(w, 401, unauthorizedError(w, "Anonymous orders must be accessed by admins"))
		return
	}

	log.Debugf("Returning %d transactions", len(order.Transactions))
	sendJSON(w, 200, order.Transactions)
}

// PaymentCreate is the endpoint for creating a payment for an order
func (a *API) PaymentCreate(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	params := &PaymentParams{Currency: "USD"}
	jsonDecoder := json.NewDecoder(r.Body)
	err := jsonDecoder.Decode(params)
	if err != nil {
		badRequestError(w, fmt.Sprintf("Could not read params: %v", err))
		return
	}

	if params.StripeToken == "" {
		badRequestError(w, "Payments requires a stripe_token")
		return
	}

	orderID := kami.Param(ctx, "order_id")
	tx := a.db.Begin()
	order := &models.Order{}

	if result := tx.Preload("LineItems").Preload("BillingAddress").First(order, "id = ?", orderID); result.Error != nil {
		tx.Rollback()
		if result.RecordNotFound() {
			notFoundError(w, "No order with this ID found")
		} else {
			internalServerError(w, fmt.Sprintf("Error during database query: %v", result.Error))
		}
		return
	}

	if order.PaymentState == models.PaidState {
		tx.Rollback()
		badRequestError(w, "This order has already been paid")
		return
	}

	if order.Currency != params.Currency {
		tx.Rollback()
		badRequestError(w, fmt.Sprintf("Currencies doesn't match - %v vs %v", order.Currency, params.Currency))
		return
	}

	token := getToken(ctx)
	if order.UserID == "" {
		if token != nil {
			claims := token.Claims.(*JWTClaims)
			order.UserID = claims.ID
			tx.Save(order)
		}
	} else {
		if token == nil {
			tx.Rollback()
			unauthorizedError(w, "You must be logged in to pay for this order")
			return
		}
		claims := token.Claims.(*JWTClaims)
		if order.UserID != claims.ID {
			tx.Rollback()
			unauthorizedError(w, "You must be logged in to pay for this order")
			return
		}
	}

	err = a.verifyAmount(ctx, order, params.Amount)
	if err != nil {
		tx.Rollback()
		internalServerError(w, fmt.Sprintf("We failed to authorize the amount for this order: %v", err))
		return
	}

	ch, err := charge.New(&stripe.ChargeParams{
		Amount:   params.Amount,
		Source:   &stripe.SourceParams{Token: params.StripeToken},
		Currency: "USD",
	})
	tr := models.NewTransaction(order)
	if ch != nil {
		tr.ProcessorID = ch.ID
	}
	if err != nil {
		tr.FailureCode = "500"
		tr.FailureDescription = err.Error()
		tr.Status = "failed"
	} else {
		tr.Status = "pending"
	}
	tx.Create(tr)

	if err != nil {
		tx.Commit()
		internalServerError(w, fmt.Sprintf("There was an error charging your card: %v", err))
		return
	}

	order.PaymentState = models.PaidState
	tx.Save(order)
	tx.Commit()

	go a.mailer.OrderConfirmationMail(tr)
	go a.mailer.OrderReceivedMail(tr)

	sendJSON(w, 200, tr)
}

// PaymentList will list all the payments that meet the criteria. It is only available to admins
func (a *API) PaymentList(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	log, _, httpErr := requireAdmin(ctx, w, "")
	if httpErr != nil {
		sendJSON(w, httpErr.Code, httpErr)
		return
	}

	query, err := parsePaymentQueryParams(a.db, r.URL.Query())
	if err != nil {
		log.WithError(err).Info("Malformed request")
		badRequestError(w, err.Error())
		return
	}

	trans, httpErr := queryForTransactions(query, log, "", "")
	if httpErr != nil {
		sendJSON(w, httpErr.Code, httpErr)
		return
	}
	sendJSON(w, 200, trans)
}

func (a *API) PaymentsView(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	log, payID, httpErr := requireAdmin(ctx, w, "pay_id")
	if httpErr != nil {
		sendJSON(w, httpErr.Code, httpErr)
		return
	}

	trans, httpErr := queryForTransactions(a.db, log, "id = ?", payID)
	if httpErr != nil {
		sendJSON(w, httpErr.Code, httpErr)
		return
	}

	if len(trans) == 0 {
		notFoundError(w, "Failed to find a transaction for ID: %s", payID)
	} else {
		sendJSON(w, 200, trans[0])
	}
}

// ------------------------------------------------------------------------------------------------
// Helpers
// ------------------------------------------------------------------------------------------------
func requireAdmin(ctx context.Context, w http.ResponseWriter, paramKey string) (*logrus.Entry, string, *HTTPError) {
	log := getLogger(ctx)
	paramValue := ""
	if paramKey != "" {
		paramValue = kami.Param(ctx, paramKey)
		log = log.WithField(paramKey, paramValue)
	}

	if !isAdmin(ctx) {
		log.Warn("Illegal access attempt")
		return nil, paramValue, unauthorizedError(w, "Can only access payments as admin")
	}

	return log, paramValue, nil
}

func (a *API) verifyAmount(ctx context.Context, order *models.Order, amount uint64) error {
	config := getConfig(ctx)

	settings := &models.SiteSettings{}
	resp, err := a.httpClient.Get(config.SiteURL + "/netlify-commerce/settings.json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		decoder := json.NewDecoder(resp.Body)
		if err := decoder.Decode(settings); err != nil {
			return err
		}
	}

	order.CalculateTotal(settings)

	if order.Total != amount {
		return fmt.Errorf("Amount calculated for order didn't match amount to charge. %v vs %v", order.Total, amount)
	}

	return nil
}

func initEndpoint(ctx context.Context, w http.ResponseWriter, paramKey string, authRequired bool) (*logrus.Entry, *JWTClaims, string, *HTTPError) {
	log := getLogger(ctx)
	paramValue := ""
	if paramKey != "" {
		paramValue = kami.Param(ctx, paramKey)
		log = log.WithField(paramKey, paramValue)
	}

	claims := getClaims(ctx)
	if claims == nil && authRequired {
		log.Warn("Claims is missing and auth required")
		return log, nil, paramValue, unauthorizedError(w, "Listing payments requires authentication")
	}

	return log, claims, paramValue, nil
}

func queryForOrder(db *gorm.DB, orderID string, log *logrus.Entry) (*models.Order, *HTTPError) {
	order := &models.Order{}
	if rsp := db.Preload("Transactions").Find(order, "id = ?", orderID); rsp.Error != nil {
		if rsp.RecordNotFound() {
			log.Infof("Failed to find order %s", orderID)
			return nil, httpError(404, "Order not found")
		}

		log.WithError(rsp.Error).Warnf("Error while querying for order %s", orderID)
		return nil, httpError(500, "Error while querying for order")
	}
	return order, nil
}

func queryForTransactions(db *gorm.DB, log *logrus.Entry, clause, id string) ([]models.Transaction, *HTTPError) {
	trans := []models.Transaction{}
	if rsp := db.Find(&trans, clause, id); rsp.Error != nil {
		if rsp.RecordNotFound() {
			log.Infof("Failed to find transactions that meet criteria '%s' '%s'", clause, id)
			return nil, httpError(404, "Transactions not found")
		}

		log.WithError(rsp.Error).Warnf("Error while querying for transactions '%s' '%s'", clause, id)
		return nil, httpError(500, "Error while querying for transactions")
	}

	return trans, nil
}
