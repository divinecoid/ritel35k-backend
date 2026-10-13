-- ============================================================ membership levels
-- name: CreateLevel :one
INSERT INTO membership_levels (id, company_id, code, name, rank, min_spend, earn_multiplier, is_default) VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING *;

-- name: ListLevels :many
SELECT * FROM membership_levels WHERE company_id = $1 ORDER BY rank;

-- name: GetLevel :one
SELECT * FROM membership_levels WHERE id = $1 AND company_id = $2;

-- name: GetDefaultLevel :one
SELECT * FROM membership_levels WHERE company_id = $1 AND is_default;

-- name: UpdateLevel :one
UPDATE membership_levels SET name = COALESCE(sqlc.narg(name), name), min_spend = COALESCE(sqlc.narg(min_spend), min_spend),
    earn_multiplier = COALESCE(sqlc.narg(earn_multiplier), earn_multiplier)
WHERE id = sqlc.arg(id) AND company_id = sqlc.arg(company_id) RETURNING *;

-- name: HighestEligibleLevel :one
SELECT * FROM membership_levels WHERE company_id = $1 AND min_spend <= $2 ORDER BY rank DESC LIMIT 1;

-- ============================================================ customers
-- name: CreateCustomer :one
INSERT INTO customers (id, company_id, member_no, name, phone, email, birthday, gender, address, notes, level_id, home_store_id, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13) RETURNING *;

-- name: GetCustomer :one
SELECT c.*, l.name AS level_name, l.rank AS level_rank, l.earn_multiplier FROM customers c JOIN membership_levels l ON l.id = c.level_id
WHERE c.id = $1 AND c.company_id = $2;

-- name: GetCustomerForUpdate :one
SELECT * FROM customers WHERE id = $1 AND company_id = $2 FOR UPDATE;

-- name: ListCustomers :many
SELECT c.*, l.name AS level_name FROM customers c JOIN membership_levels l ON l.id = c.level_id
WHERE c.company_id = sqlc.arg(company_id)
  AND (sqlc.arg(q)::text = '' OR c.name ILIKE '%' || sqlc.arg(q)::text || '%' OR c.phone LIKE '%' || sqlc.arg(q)::text || '%' OR c.member_no ILIKE sqlc.arg(q)::text || '%' OR c.email ILIKE '%' || sqlc.arg(q)::text || '%')
  AND (sqlc.narg(level_id)::uuid IS NULL OR c.level_id = sqlc.narg(level_id)::uuid)
  AND (sqlc.arg(status)::text = '' OR c.status = sqlc.arg(status)::text)
ORDER BY c.name
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: CountCustomers :one
SELECT count(*) FROM customers c
WHERE c.company_id = sqlc.arg(company_id)
  AND (sqlc.arg(q)::text = '' OR c.name ILIKE '%' || sqlc.arg(q)::text || '%' OR c.phone LIKE '%' || sqlc.arg(q)::text || '%' OR c.member_no ILIKE sqlc.arg(q)::text || '%' OR c.email ILIKE '%' || sqlc.arg(q)::text || '%')
  AND (sqlc.narg(level_id)::uuid IS NULL OR c.level_id = sqlc.narg(level_id)::uuid)
  AND (sqlc.arg(status)::text = '' OR c.status = sqlc.arg(status)::text);

-- name: UpdateCustomer :one
UPDATE customers SET name = COALESCE(sqlc.narg(name), name), email = COALESCE(sqlc.narg(email), email), birthday = COALESCE(sqlc.narg(birthday), birthday),
    gender = COALESCE(sqlc.narg(gender), gender), address = COALESCE(sqlc.narg(address), address), notes = COALESCE(sqlc.narg(notes), notes),
    status = COALESCE(sqlc.narg(status), status), level_id = COALESCE(sqlc.narg(level_id), level_id), updated_at = now()
WHERE id = sqlc.arg(id) AND company_id = sqlc.arg(company_id) RETURNING *;

-- name: ChangePoints :one
UPDATE customers SET points_balance = points_balance + $2, updated_at = now() WHERE id = $1 RETURNING points_balance;

-- name: AddCustomerSpend :one
UPDATE customers SET lifetime_spend = lifetime_spend + $2, last_purchase_at = now(), updated_at = now() WHERE id = $1 RETURNING *;

-- name: SetCustomerLevel :exec
UPDATE customers SET level_id = $2, updated_at = now() WHERE id = $1;

-- name: InsertLoyalty :exec
INSERT INTO loyalty_transactions (id, company_id, customer_id, kind, points, balance_after, sale_id, expires_at, note, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);

-- name: ListLoyalty :many
SELECT * FROM loyalty_transactions WHERE customer_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2;

-- name: LoyaltyForSale :many
SELECT * FROM loyalty_transactions WHERE sale_id = $1 AND kind IN ('earn','redeem') ORDER BY created_at;

-- name: ExpirableCustomers :many
-- Points earned more than `expiry` ago that have neither been spent nor already expired (FIFO approximation).
SELECT c.id AS customer_id, c.company_id, c.points_balance,
       COALESCE(SUM(l.points) FILTER (WHERE l.kind = 'earn' AND l.expires_at <= sqlc.arg(now)::timestamptz), 0)::bigint AS earned_expired,
       COALESCE(-SUM(l.points) FILTER (WHERE l.kind IN ('redeem','restore_redeem')), 0)::bigint AS net_redeemed,
       COALESCE(-SUM(l.points) FILTER (WHERE l.kind = 'expire'), 0)::bigint AS already_expired
FROM customers c JOIN loyalty_transactions l ON l.customer_id = c.id
WHERE c.points_balance > 0
GROUP BY c.id
HAVING COALESCE(SUM(l.points) FILTER (WHERE l.kind = 'earn' AND l.expires_at <= sqlc.arg(now)::timestamptz), 0) > 0;

-- name: CustomerSales :many
SELECT s.id, s.document_number, s.status, s.grand_total, s.points_earned, s.points_redeemed, s.created_at, st.name AS store_name
FROM sales s JOIN stores st ON st.id = s.store_id WHERE s.customer_id = $1 ORDER BY s.created_at DESC LIMIT $2;

-- ============================================================ promotions
-- name: InsertPromotion :one
INSERT INTO promotions (id, company_id, code, name, description, type, config, priority, stackable, exclusive, starts_at, ends_at, days_of_week, time_from, time_to, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16) RETURNING *;

-- name: GetPromotion :one
SELECT * FROM promotions WHERE id = $1 AND company_id = $2;

-- name: ListPromotions :many
SELECT * FROM promotions WHERE company_id = sqlc.arg(company_id)
  AND (sqlc.arg(status)::text = '' OR status = sqlc.arg(status)::text)
  AND (sqlc.arg(q)::text = '' OR name ILIKE '%' || sqlc.arg(q)::text || '%' OR code ILIKE '%' || sqlc.arg(q)::text || '%')
ORDER BY priority, name;

-- name: UpdatePromotion :one
UPDATE promotions SET name = $3, description = $4, config = $5, priority = $6, stackable = $7, exclusive = $8, starts_at = $9, ends_at = $10,
    days_of_week = $11, time_from = $12, time_to = $13, status = $14, version = version + 1, updated_at = now()
WHERE id = $1 AND company_id = $2 RETURNING *;

-- name: DeletePromotionScopes :exec
DELETE FROM promotion_scopes WHERE promotion_id = $1;

-- name: InsertPromotionScope :exec
INSERT INTO promotion_scopes (promotion_id, scope_type, scope_id) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING;

-- name: ListPromotionScopes :many
SELECT * FROM promotion_scopes WHERE promotion_id = ANY(sqlc.arg(ids)::uuid[]);

-- name: ListActivePromotions :many
-- Active promotions for a store: status, validity window, and org-unit scope (store must sit below one of the scoped units).
SELECT p.* FROM promotions p
WHERE p.company_id = sqlc.arg(company_id) AND p.status = 'active'
  AND (p.starts_at IS NULL OR p.starts_at <= sqlc.arg(at)::timestamptz) AND (p.ends_at IS NULL OR p.ends_at > sqlc.arg(at)::timestamptz)
  AND (NOT EXISTS (SELECT 1 FROM promotion_scopes s WHERE s.promotion_id = p.id AND s.scope_type = 'org_unit')
       OR EXISTS (SELECT 1 FROM promotion_scopes s JOIN org_units so ON so.id = s.scope_id JOIN org_units t ON t.id = sqlc.arg(store_org_unit_id)
                  WHERE s.promotion_id = p.id AND s.scope_type = 'org_unit' AND t.path <@ so.path));

-- name: InsertSaleItemPromotion :exec
INSERT INTO sale_item_promotions (sale_item_id, promotion_id, name, amount) VALUES ($1, $2, $3, $4);

-- name: ListSaleItemPromotions :many
SELECT sip.* FROM sale_item_promotions sip JOIN sale_items i ON i.id = sip.sale_item_id WHERE i.sale_id = $1;

-- ============================================================ vouchers
-- name: InsertVoucher :one
INSERT INTO vouchers (id, company_id, code, kind, value, min_spend, max_discount, max_uses, customer_id, valid_from, valid_to, batch, note, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14) RETURNING *;

-- name: GetVoucherByCode :one
SELECT * FROM vouchers WHERE company_id = $1 AND code = $2;

-- name: GetVoucher :one
SELECT * FROM vouchers WHERE id = $1 AND company_id = $2;

-- name: ListVouchers :many
SELECT v.*, c.name AS customer_name FROM vouchers v LEFT JOIN customers c ON c.id = v.customer_id
WHERE v.company_id = sqlc.arg(company_id)
  AND (sqlc.arg(q)::text = '' OR v.code ILIKE '%' || sqlc.arg(q)::text || '%' OR v.batch ILIKE '%' || sqlc.arg(q)::text || '%')
  AND (sqlc.arg(status)::text = '' OR v.status = sqlc.arg(status)::text)
ORDER BY v.created_at DESC
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: CountVouchers :one
SELECT count(*) FROM vouchers v
WHERE v.company_id = sqlc.arg(company_id)
  AND (sqlc.arg(q)::text = '' OR v.code ILIKE '%' || sqlc.arg(q)::text || '%' OR v.batch ILIKE '%' || sqlc.arg(q)::text || '%')
  AND (sqlc.arg(status)::text = '' OR v.status = sqlc.arg(status)::text);

-- name: SetVoucherStatus :one
UPDATE vouchers SET status = $3 WHERE id = $1 AND company_id = $2 RETURNING *;

-- name: ClaimVoucher :one
-- Atomic usage claim: concurrent checkouts cannot exceed max_uses.
UPDATE vouchers SET used_count = used_count + 1
WHERE id = $1 AND status = 'active' AND (max_uses IS NULL OR used_count < max_uses) RETURNING *;

-- name: ReleaseVoucher :exec
UPDATE vouchers SET used_count = GREATEST(used_count - 1, 0) WHERE id = $1;

-- name: InsertRedemption :exec
INSERT INTO voucher_redemptions (id, voucher_id, sale_id, customer_id, amount) VALUES ($1, $2, $3, $4, $5);

-- name: ActiveRedemptionForSale :one
SELECT * FROM voucher_redemptions WHERE sale_id = $1 AND reversed_at IS NULL;

-- name: ReverseRedemption :exec
UPDATE voucher_redemptions SET reversed_at = now() WHERE id = $1;

-- name: CustomerVoucherUses :one
SELECT count(*) FROM voucher_redemptions WHERE voucher_id = $1 AND customer_id = $2 AND reversed_at IS NULL;

-- name: ReduceCustomerSpend :exec
UPDATE customers SET lifetime_spend = GREATEST(lifetime_spend - $2, 0), updated_at = now() WHERE id = $1;

-- name: ReversedEarnForSale :one
SELECT COALESCE(-SUM(points), 0)::bigint FROM loyalty_transactions WHERE sale_id = $1 AND kind = 'reverse_earn';
