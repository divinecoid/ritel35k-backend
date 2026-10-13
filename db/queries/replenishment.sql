-- name: ReplenishmentRows :many
-- Per store+warehouse+SKU: stock position and what sold in the window, with the cheapest active supplier. Only SKUs that
-- sold in the window or sit at/below their reorder point are candidates.
WITH demand AS (
    SELECT s.store_id, si.variant_id, sum(si.qty)::numeric AS sold,
           sum(CASE WHEN s.business_date >= sqlc.arg(recent_from)::date THEN si.qty ELSE 0 END)::numeric AS sold_recent
    FROM sale_items si JOIN sales s ON s.id = si.sale_id
    WHERE s.company_id = sqlc.arg(company_id) AND s.status = 'completed'
      AND s.business_date >= sqlc.arg(from_date)::date AND s.business_date <= sqlc.arg(to_date)::date
    GROUP BY s.store_id, si.variant_id
)
SELECT b.store_id, st.code AS store_code, st.name AS store_name, b.warehouse_id, w.code AS warehouse_code, b.variant_id, v.sku,
       p.name AS product_name, v.name AS variant_name, v.reorder_point, v.min_stock, v.max_stock, v.purchase_price AS variant_price,
       b.on_hand, b.in_transit,
       COALESCE(d.sold, 0)::numeric AS sold, COALESCE(d.sold_recent, 0)::numeric AS sold_recent,
       COALESCE(sup.supplier_id, '00000000-0000-0000-0000-000000000000'::uuid) AS supplier_id, COALESCE(sup.supplier_name, '')::text AS supplier_name,
       COALESCE(sup.price, 0)::numeric AS supplier_price, COALESCE(sup.moq, 1)::numeric AS moq, COALESCE(sup.lead_days, 0)::int AS lead_days
FROM inventory_balances b
JOIN product_variants v ON v.id = b.variant_id AND v.status = 'active'
JOIN products p ON p.id = v.product_id
JOIN warehouses w ON w.id = b.warehouse_id AND w.status = 'active'
JOIN stores st ON st.id = b.store_id
JOIN org_units o ON o.id = st.org_unit_id
LEFT JOIN demand d ON d.store_id = b.store_id AND d.variant_id = b.variant_id
LEFT JOIN LATERAL (
    SELECT sp.supplier_id, su.name AS supplier_name, sp.purchase_price AS price, sp.moq, GREATEST(sp.lead_time_days, su.lead_time_days) AS lead_days
    FROM supplier_products sp JOIN suppliers su ON su.id = sp.supplier_id AND su.status = 'active'
    WHERE sp.variant_id = v.id
    ORDER BY sp.purchase_price, GREATEST(sp.lead_time_days, su.lead_time_days) LIMIT 1
) sup ON true
WHERE b.company_id = sqlc.arg(company_id) AND b.store_id IS NOT NULL
  AND (sqlc.arg(all_scope)::bool OR EXISTS (SELECT 1 FROM unnest(sqlc.arg(scope_paths)::text[]) sp WHERE o.path <@ sp::ltree))
  AND (sqlc.narg(store_id)::uuid IS NULL OR b.store_id = sqlc.narg(store_id)::uuid)
  AND (COALESCE(d.sold, 0) > 0 OR (v.reorder_point > 0 AND b.on_hand <= v.reorder_point))
ORDER BY st.name, p.name, v.sku
LIMIT sqlc.arg(row_limit);
