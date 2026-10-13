-- name: CreateCategory :one
INSERT INTO categories (id, company_id, parent_id, code, name, created_by) VALUES ($1, $2, $3, $4, $5, $6) RETURNING *;

-- name: ListCategories :many
SELECT * FROM categories WHERE company_id = $1 ORDER BY name;

-- name: UpdateCategory :one
UPDATE categories SET
    name = COALESCE(sqlc.narg(name), name),
    parent_id = COALESCE(sqlc.narg(parent_id), parent_id),
    status = COALESCE(sqlc.narg(status), status),
    updated_at = now()
WHERE id = sqlc.arg(id) AND company_id = sqlc.arg(company_id) RETURNING *;

-- name: CreateBrand :one
INSERT INTO brands (id, company_id, code, name, created_by) VALUES ($1, $2, $3, $4, $5) RETURNING *;

-- name: ListBrands :many
SELECT * FROM brands WHERE company_id = $1 ORDER BY name;

-- name: UpdateBrand :one
UPDATE brands SET
    name = COALESCE(sqlc.narg(name), name),
    status = COALESCE(sqlc.narg(status), status),
    updated_at = now()
WHERE id = sqlc.arg(id) AND company_id = sqlc.arg(company_id) RETURNING *;

-- name: CreateUOM :one
INSERT INTO uoms (id, company_id, code, name, allows_decimal, created_by) VALUES ($1, $2, $3, $4, $5, $6) RETURNING *;

-- name: ListUOMs :many
SELECT * FROM uoms WHERE company_id = $1 ORDER BY code;

-- name: CreateProduct :one
INSERT INTO products (id, company_id, code, name, short_name, category_id, brand_id, uom_id, tax_code, image_key, is_rfid_tracked, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
RETURNING *;

-- name: GetProduct :one
SELECT * FROM products WHERE id = $1 AND company_id = $2;

-- name: ListProducts :many
SELECT p.* FROM products p
WHERE p.company_id = sqlc.arg(company_id)
  AND (sqlc.arg(q)::text = '' OR p.name ILIKE '%' || sqlc.arg(q)::text || '%' OR p.code ILIKE '%' || sqlc.arg(q)::text || '%'
       OR EXISTS (SELECT 1 FROM product_variants v LEFT JOIN barcodes b ON b.variant_id = v.id
                  WHERE v.product_id = p.id AND (v.sku ILIKE '%' || sqlc.arg(q)::text || '%' OR b.barcode = sqlc.arg(q)::text)))
  AND (sqlc.narg(category_id)::uuid IS NULL OR p.category_id = sqlc.narg(category_id)::uuid)
  AND (sqlc.narg(brand_id)::uuid IS NULL OR p.brand_id = sqlc.narg(brand_id)::uuid)
  AND (sqlc.arg(status)::text = '' OR p.status = sqlc.arg(status)::text)
ORDER BY p.name
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: CountProducts :one
SELECT count(*) FROM products p
WHERE p.company_id = sqlc.arg(company_id)
  AND (sqlc.arg(q)::text = '' OR p.name ILIKE '%' || sqlc.arg(q)::text || '%' OR p.code ILIKE '%' || sqlc.arg(q)::text || '%'
       OR EXISTS (SELECT 1 FROM product_variants v LEFT JOIN barcodes b ON b.variant_id = v.id
                  WHERE v.product_id = p.id AND (v.sku ILIKE '%' || sqlc.arg(q)::text || '%' OR b.barcode = sqlc.arg(q)::text)))
  AND (sqlc.narg(category_id)::uuid IS NULL OR p.category_id = sqlc.narg(category_id)::uuid)
  AND (sqlc.narg(brand_id)::uuid IS NULL OR p.brand_id = sqlc.narg(brand_id)::uuid)
  AND (sqlc.arg(status)::text = '' OR p.status = sqlc.arg(status)::text);

-- name: UpdateProduct :one
UPDATE products SET
    name = COALESCE(sqlc.narg(name), name),
    short_name = COALESCE(sqlc.narg(short_name), short_name),
    category_id = COALESCE(sqlc.narg(category_id), category_id),
    brand_id = COALESCE(sqlc.narg(brand_id), brand_id),
    uom_id = COALESCE(sqlc.narg(uom_id), uom_id),
    tax_code = COALESCE(sqlc.narg(tax_code), tax_code),
    image_key = COALESCE(sqlc.narg(image_key), image_key),
    is_rfid_tracked = COALESCE(sqlc.narg(is_rfid_tracked), is_rfid_tracked),
    status = COALESCE(sqlc.narg(status), status),
    version = version + 1,
    updated_at = now()
WHERE id = sqlc.arg(id) AND company_id = sqlc.arg(company_id) AND version = sqlc.arg(version)
RETURNING *;

-- name: CreateVariant :one
INSERT INTO product_variants (id, company_id, product_id, sku, name, attributes, purchase_price, cost, min_stock, max_stock, reorder_point, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
RETURNING *;

-- name: GetVariant :one
SELECT * FROM product_variants WHERE id = $1 AND company_id = $2;

-- name: ListVariantsByProducts :many
SELECT * FROM product_variants WHERE product_id = ANY(sqlc.arg(product_ids)::uuid[]) ORDER BY sku;

-- name: UpdateVariant :one
UPDATE product_variants SET
    name = COALESCE(sqlc.narg(name), name),
    attributes = COALESCE(sqlc.narg(attributes), attributes),
    purchase_price = COALESCE(sqlc.narg(purchase_price), purchase_price),
    cost = COALESCE(sqlc.narg(cost), cost),
    min_stock = COALESCE(sqlc.narg(min_stock), min_stock),
    max_stock = COALESCE(sqlc.narg(max_stock), max_stock),
    reorder_point = COALESCE(sqlc.narg(reorder_point), reorder_point),
    status = COALESCE(sqlc.narg(status), status),
    updated_at = now()
WHERE id = sqlc.arg(id) AND company_id = sqlc.arg(company_id)
RETURNING *;

-- name: CreateBarcode :one
INSERT INTO barcodes (id, company_id, variant_id, barcode, kind, is_primary) VALUES ($1, $2, $3, $4, $5, $6) RETURNING *;

-- name: ListBarcodesByVariants :many
SELECT * FROM barcodes WHERE variant_id = ANY(sqlc.arg(variant_ids)::uuid[]) ORDER BY is_primary DESC, barcode;

-- name: DeleteBarcode :execrows
DELETE FROM barcodes WHERE id = $1 AND company_id = $2;

-- name: LookupBarcode :one
SELECT b.barcode, v.id AS variant_id, v.sku, v.name AS variant_name, p.id AS product_id, p.name AS product_name, p.uom_id, p.status AS product_status, v.status AS variant_status
FROM barcodes b
JOIN product_variants v ON v.id = b.variant_id
JOIN products p ON p.id = v.product_id
WHERE b.company_id = $1 AND b.barcode = $2;

-- name: CreateSupplier :one
INSERT INTO suppliers (id, company_id, code, name, pic, phone, email, address, npwp, payment_term_days, bank_account, lead_time_days, currency, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
RETURNING *;

-- name: GetSupplier :one
SELECT * FROM suppliers WHERE id = $1 AND company_id = $2;

-- name: ListSuppliers :many
SELECT * FROM suppliers
WHERE company_id = sqlc.arg(company_id)
  AND (sqlc.arg(q)::text = '' OR name ILIKE '%' || sqlc.arg(q)::text || '%' OR code ILIKE '%' || sqlc.arg(q)::text || '%')
ORDER BY name
LIMIT sqlc.arg(page_limit) OFFSET sqlc.arg(page_offset);

-- name: CountSuppliers :one
SELECT count(*) FROM suppliers
WHERE company_id = sqlc.arg(company_id)
  AND (sqlc.arg(q)::text = '' OR name ILIKE '%' || sqlc.arg(q)::text || '%' OR code ILIKE '%' || sqlc.arg(q)::text || '%');

-- name: UpdateSupplier :one
UPDATE suppliers SET
    name = COALESCE(sqlc.narg(name), name),
    pic = COALESCE(sqlc.narg(pic), pic),
    phone = COALESCE(sqlc.narg(phone), phone),
    email = COALESCE(sqlc.narg(email), email),
    address = COALESCE(sqlc.narg(address), address),
    npwp = COALESCE(sqlc.narg(npwp), npwp),
    payment_term_days = COALESCE(sqlc.narg(payment_term_days), payment_term_days),
    bank_account = COALESCE(sqlc.narg(bank_account), bank_account),
    lead_time_days = COALESCE(sqlc.narg(lead_time_days), lead_time_days),
    currency = COALESCE(sqlc.narg(currency), currency),
    status = COALESCE(sqlc.narg(status), status),
    updated_at = now()
WHERE id = sqlc.arg(id) AND company_id = sqlc.arg(company_id)
RETURNING *;

-- name: UpsertSupplierProduct :one
INSERT INTO supplier_products (supplier_id, variant_id, company_id, supplier_sku, purchase_price, moq, lead_time_days)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (supplier_id, variant_id) DO UPDATE
    SET supplier_sku = EXCLUDED.supplier_sku, purchase_price = EXCLUDED.purchase_price, moq = EXCLUDED.moq, lead_time_days = EXCLUDED.lead_time_days
RETURNING *;

-- name: ListSupplierProducts :many
SELECT sp.*, v.sku, v.name AS variant_name FROM supplier_products sp
JOIN product_variants v ON v.id = sp.variant_id
WHERE sp.supplier_id = $1 AND sp.company_id = $2
ORDER BY v.sku;

-- name: DeleteSupplierProduct :execrows
DELETE FROM supplier_products WHERE supplier_id = $1 AND variant_id = $2 AND company_id = $3;
