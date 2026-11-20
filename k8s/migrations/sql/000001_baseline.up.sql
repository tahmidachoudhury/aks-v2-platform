-- Baseline: the schema the five owning services create in migrate() at
-- startup, copied verbatim, plus notification-service's template seed.
-- Everything is IF NOT EXISTS or ON CONFLICT DO NOTHING, so this applies
-- cleanly whether the services or this migration ran first, and the
-- services' own migrate() stays a no-op on a migrated database.
--
-- Never edit this file. Every schema change from here on is a new numbered
-- file in this folder (see ../README.md), and the services' migrate()
-- functions stay frozen until they are removed.

-- Give up on a table lock after 10s rather than queue behind a long
-- transaction and block the services' queries behind this one. Every file
-- starts with this line. It is not in the connection URL, as there it would
-- also cut golang-migrate's own wait for the advisory lock to 10s.
SET lock_timeout = '10s';

-- order-service
CREATE TABLE IF NOT EXISTS orders (
	id SERIAL PRIMARY KEY,
	customer_id VARCHAR(255) NOT NULL,
	status VARCHAR(20) NOT NULL DEFAULT 'pending',
	items JSONB NOT NULL,
	total DECIMAL(12,2) NOT NULL DEFAULT 0,
	currency VARCHAR(3) NOT NULL DEFAULT 'GBP',
	notes TEXT,
	created_at TIMESTAMP DEFAULT NOW(),
	updated_at TIMESTAMP DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_orders_customer ON orders(customer_id);
CREATE INDEX IF NOT EXISTS idx_orders_status ON orders(status);
CREATE TABLE IF NOT EXISTS order_events (
	id SERIAL PRIMARY KEY,
	order_id INTEGER NOT NULL REFERENCES orders(id),
	event_type VARCHAR(50) NOT NULL,
	old_status VARCHAR(20),
	new_status VARCHAR(20),
	metadata JSONB,
	created_at TIMESTAMP DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_order_events_order ON order_events(order_id);

-- inventory-service
CREATE TABLE IF NOT EXISTS products (
	id VARCHAR(50) PRIMARY KEY,
	name VARCHAR(255) NOT NULL,
	sku VARCHAR(100) UNIQUE NOT NULL,
	price DECIMAL(10,2) NOT NULL,
	stock INTEGER NOT NULL DEFAULT 0,
	reserved INTEGER NOT NULL DEFAULT 0,
	created_at TIMESTAMP DEFAULT NOW(),
	updated_at TIMESTAMP DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_products_sku ON products(sku);
CREATE TABLE IF NOT EXISTS reservations (
	id SERIAL PRIMARY KEY,
	order_id INTEGER NOT NULL,
	product_id VARCHAR(50) NOT NULL REFERENCES products(id),
	quantity INTEGER NOT NULL,
	status VARCHAR(20) NOT NULL DEFAULT 'active',
	expires_at TIMESTAMP NOT NULL,
	created_at TIMESTAMP DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_reservations_order ON reservations(order_id);
CREATE INDEX IF NOT EXISTS idx_reservations_status ON reservations(status);

-- payment-service
CREATE TABLE IF NOT EXISTS payments (
	id VARCHAR(50) PRIMARY KEY,
	order_id INTEGER NOT NULL,
	customer_id VARCHAR(255) NOT NULL,
	amount DECIMAL(12,2) NOT NULL,
	currency VARCHAR(3) NOT NULL DEFAULT 'GBP',
	status VARCHAR(20) NOT NULL DEFAULT 'pending',
	method VARCHAR(50),
	reference VARCHAR(255),
	created_at TIMESTAMP DEFAULT NOW(),
	updated_at TIMESTAMP DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_payments_order ON payments(order_id);
CREATE INDEX IF NOT EXISTS idx_payments_customer ON payments(customer_id);
CREATE TABLE IF NOT EXISTS ledger_entries (
	id SERIAL PRIMARY KEY,
	payment_id VARCHAR(50) NOT NULL REFERENCES payments(id),
	entry_type VARCHAR(20) NOT NULL,
	debit DECIMAL(12,2) NOT NULL DEFAULT 0,
	credit DECIMAL(12,2) NOT NULL DEFAULT 0,
	currency VARCHAR(3) NOT NULL DEFAULT 'GBP',
	description TEXT,
	created_at TIMESTAMP DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_ledger_payment ON ledger_entries(payment_id);

-- notification-service
CREATE TABLE IF NOT EXISTS notifications (
	id SERIAL PRIMARY KEY,
	recipient VARCHAR(255) NOT NULL,
	channel VARCHAR(20) NOT NULL,
	template VARCHAR(100) NOT NULL,
	subject TEXT,
	body TEXT NOT NULL,
	metadata JSONB,
	status VARCHAR(20) NOT NULL DEFAULT 'pending',
	sent_at TIMESTAMP,
	created_at TIMESTAMP DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_notifications_recipient ON notifications(recipient);
CREATE INDEX IF NOT EXISTS idx_notifications_status ON notifications(status);
CREATE TABLE IF NOT EXISTS notification_templates (
	id VARCHAR(100) PRIMARY KEY,
	channel VARCHAR(20) NOT NULL,
	subject TEXT,
	body TEXT NOT NULL,
	created_at TIMESTAMP DEFAULT NOW(),
	updated_at TIMESTAMP DEFAULT NOW()
);
INSERT INTO notification_templates (id, channel, subject, body) VALUES
	('order_confirmed', 'email', 'Order Confirmed - #{{.OrderID}}', 'Hi {{.CustomerName}}, your order #{{.OrderID}} has been confirmed. Total: {{.Currency}} {{.Total}}'),
	('order_shipped', 'email', 'Your Order Has Shipped - #{{.OrderID}}', 'Good news! Your order #{{.OrderID}} has been shipped. Tracking: {{.TrackingNumber}}'),
	('order_delivered', 'email', 'Order Delivered - #{{.OrderID}}', 'Your order #{{.OrderID}} has been delivered. Thank you for your purchase!'),
	('payment_failed', 'email', 'Payment Failed - Order #{{.OrderID}}', 'We were unable to process your payment for order #{{.OrderID}}. Please update your payment method.'),
	('low_stock_alert', 'email', 'Low Stock Alert - {{.ProductName}}', 'Product {{.ProductName}} ({{.SKU}}) is running low. Available: {{.Available}}'),
	('order_confirmed_sms', 'sms', '', 'Order #{{.OrderID}} confirmed. Total: {{.Currency}} {{.Total}}'),
	('order_shipped_sms', 'sms', '', 'Order #{{.OrderID}} shipped. Track: {{.TrackingNumber}}')
ON CONFLICT (id) DO NOTHING;

-- shipping-service
CREATE TABLE IF NOT EXISTS shipments (
	id SERIAL PRIMARY KEY,
	order_id INTEGER NOT NULL,
	carrier VARCHAR(50) NOT NULL,
	tracking_number VARCHAR(100) UNIQUE,
	status VARCHAR(30) NOT NULL DEFAULT 'label_created',
	recipient_name VARCHAR(255),
	address_line1 TEXT,
	address_line2 TEXT,
	city VARCHAR(100),
	postcode VARCHAR(20),
	country VARCHAR(2) DEFAULT 'GB',
	weight_kg DECIMAL(8,2),
	estimated_delivery DATE,
	shipped_at TIMESTAMP,
	delivered_at TIMESTAMP,
	created_at TIMESTAMP DEFAULT NOW(),
	updated_at TIMESTAMP DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_shipments_order ON shipments(order_id);
CREATE INDEX IF NOT EXISTS idx_shipments_tracking ON shipments(tracking_number);
CREATE TABLE IF NOT EXISTS tracking_events (
	id SERIAL PRIMARY KEY,
	shipment_id INTEGER NOT NULL REFERENCES shipments(id),
	status VARCHAR(30) NOT NULL,
	location VARCHAR(255),
	description TEXT,
	occurred_at TIMESTAMP NOT NULL,
	created_at TIMESTAMP DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_tracking_shipment ON tracking_events(shipment_id);
