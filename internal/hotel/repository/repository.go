package repository

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"booking-api-demo/internal/hotel/model"

	"booking-api-demo/internal/helper/pagination"

	"github.com/lib/pq"
)

type (
	hotelRepository struct {
		db           *sql.DB
		queryTimeout time.Duration
	}
)

// New creates and returns a new instance of hotelRepository.
func New(db *sql.DB, queryTimeout time.Duration) *hotelRepository {
	return &hotelRepository{
		db:           db,
		queryTimeout: queryTimeout,
	}
}

// GetHotelsByName finds hotels by their name from the DB.
func (r *hotelRepository) GetHotelsByName(ctx context.Context, p model.FindHotelsParams) (*model.HotelsPage, error) {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	const query = `
		SELECT id, name, address, phone
		FROM hotels
		WHERE name ILIKE '%' || $1 || '%'
		  AND ($2 = '' OR id > $2)
		ORDER BY id
		LIMIT $3
	`

	rows, err := r.db.QueryContext(ctx, query, p.NamePattern, p.AfterID, p.PageSize+1)
	if err != nil {
		return nil, fmt.Errorf("find hotels by name: %w", err)
	}
	defer rows.Close()

	hotels := make([]*model.HotelItem, 0, p.PageSize)
	for rows.Next() {
		var h model.HotelItem
		if err := rows.Scan(&h.ID, &h.Name, &h.Address, &h.Phone); err != nil {
			return nil, fmt.Errorf("scan hotel: %w", err)
		}
		hotels = append(hotels, &h)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate hotels: %w", err)
	}

	page := &model.HotelsPage{
		Hotels:   hotels,
		PageSize: p.PageSize,
	}

	if len(hotels) > p.PageSize {
		page.Hotels = hotels[:p.PageSize]

		nextPage, err := pagination.Encode(page.Hotels[len(page.Hotels)-1].ID)
		if err != nil {
			return nil, fmt.Errorf("encode page token: %w", err)
		}
		page.NextPage = nextPage
	}

	return page, nil
}

// PutHotels inserts the provided hotels in a single statement.
func (r *hotelRepository) PutHotels(ctx context.Context, hotels []*model.HotelItem) error {
	if len(hotels) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	const query = `
		INSERT INTO hotels (id, name, address, phone)
		SELECT * FROM unnest($1::text[], $2::text[], $3::text[], $4::text[])
	`

	var (
		ids       = make([]string, len(hotels))
		names     = make([]string, len(hotels))
		addresses = make([]string, len(hotels))
		phones    = make([]string, len(hotels))
	)

	for i, h := range hotels {
		ids[i] = h.ID
		names[i] = h.Name
		addresses[i] = h.Address
		phones[i] = h.Phone
	}

	if _, err := r.db.ExecContext(
		ctx,
		query,
		pq.Array(ids),
		pq.Array(names),
		pq.Array(addresses),
		pq.Array(phones),
	); err != nil {
		return fmt.Errorf("insert hotels: %w", err)
	}

	return nil
}

// DeleteAllHotels removes all hotels together with their dependent records.
func (r *hotelRepository) DeleteAllHotels(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	if _, err := r.db.ExecContext(ctx, `DELETE FROM hotels`); err != nil {
		return fmt.Errorf("delete all hotels: %w", err)
	}

	return nil
}
