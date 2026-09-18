package repository

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	modelReservation "booking-api-demo/internal/reservation/model"
	modelRoom "booking-api-demo/internal/room/model"

	"booking-api-demo/internal/helper/pagination"

	"github.com/lib/pq"
)

type (
	roomRepository struct {
		db           *sql.DB
		queryTimeout time.Duration
	}
)

// New creates a new instance of the room repository.
func New(db *sql.DB, queryTimeout time.Duration) *roomRepository {
	return &roomRepository{
		db:           db,
		queryTimeout: queryTimeout,
	}
}

// GetAvailableRooms retrieves all available rooms based on params provided.
func (r *roomRepository) GetAvailableRooms(ctx context.Context, p modelRoom.FindAvailableRoomsParams) (*modelRoom.RoomsPage, error) {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	const query = `
		WITH free_rooms AS (
		    SELECT r.id, r.hotel_id, r.room_label, r.is_available,
		           rt.id AS room_type_id, rt.caption, rt.capacity
		    FROM rooms AS r
		    INNER JOIN room_types AS rt ON rt.id = r.room_type_id
		    WHERE r.is_available
		      AND NOT EXISTS (
		          SELECT 1
		          FROM reservation_rooms AS rr
		          INNER JOIN reservations AS res ON res.id = rr.reservation_id
		          WHERE rr.room_id = r.id
		            AND res.reservation_status <> $4
		            AND res.check_in_date < $6::date
		            AND res.check_out_date > $5::date
		      )
		),
		hotel_free_capacity AS (
		    SELECT hotel_id, SUM(capacity) AS total_capacity
		    FROM free_rooms
		    GROUP BY hotel_id
		)
		SELECT fr.id, fr.hotel_id, fr.room_label, fr.is_available,
		       fr.room_type_id, fr.caption, fr.capacity,
		       h.name, h.address
		FROM free_rooms AS fr
		INNER JOIN hotels AS h ON h.id = fr.hotel_id
		INNER JOIN hotel_free_capacity AS hfc ON hfc.hotel_id = fr.hotel_id
		WHERE hfc.total_capacity >= $1
		  AND ($2 = '' OR fr.hotel_id = $2)
		  AND ($3 = '' OR fr.id > $3)
		ORDER BY fr.id
		LIMIT $7
	`

	rows, err := r.db.QueryContext(
		ctx,
		query,
		p.GuestsCount,
		p.HotelID,
		p.AfterID,
		int(modelReservation.ReservationStatusCancelled),
		p.CheckInDate,
		p.CheckOutDate,
		p.PageSize+1,
	)
	if err != nil {
		return nil, fmt.Errorf("find available rooms: %w", err)
	}
	defer rows.Close()

	rooms := make([]*modelRoom.RoomItem, 0, p.PageSize)
	for rows.Next() {
		var (
			room      modelRoom.RoomItem
			roomType  modelRoom.RoomType
			hotelInfo modelRoom.RoomItemHotelDetails
		)
		if err := rows.Scan(
			&room.ID,
			&room.HotelID,
			&room.Label,
			&room.IsAvailable,
			&roomType.ID,
			&roomType.Caption,
			&roomType.Capacity,
			&hotelInfo.Name,
			&hotelInfo.Address,
		); err != nil {
			return nil, fmt.Errorf("scan room: %w", err)
		}

		room.TypeID = roomType.ID
		room.RoomType = &roomType
		room.RoomItemHotelDetails = &hotelInfo
		rooms = append(rooms, &room)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rooms: %w", err)
	}

	page := &modelRoom.RoomsPage{
		Rooms:    rooms,
		PageSize: p.PageSize,
	}

	if len(rooms) > p.PageSize {
		page.Rooms = rooms[:p.PageSize]

		nextPage, err := pagination.Encode(page.Rooms[len(page.Rooms)-1].ID)
		if err != nil {
			return nil, fmt.Errorf("encode page token: %w", err)
		}
		page.NextPage = nextPage
	}

	return page, nil
}

// GetRoomTypes retrieves all available room types.
func (r *roomRepository) GetRoomTypes(ctx context.Context) ([]*modelRoom.RoomType, error) {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	const query = `
		SELECT id, caption, capacity
		FROM room_types
		ORDER BY id
	`

	rows, err := r.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("get room types: %w", err)
	}
	defer rows.Close()

	roomTypes := make([]*modelRoom.RoomType, 0)
	for rows.Next() {
		var roomType modelRoom.RoomType
		if err := rows.Scan(&roomType.ID, &roomType.Caption, &roomType.Capacity); err != nil {
			return nil, fmt.Errorf("scan room type: %w", err)
		}
		roomTypes = append(roomTypes, &roomType)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate room types: %w", err)
	}

	return roomTypes, nil
}

// PutRooms inserts the provided rooms in a single statement.
func (r *roomRepository) PutRooms(ctx context.Context, rooms []*modelRoom.RoomItem) error {
	if len(rooms) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	const query = `
		INSERT INTO rooms (id, hotel_id, room_type_id, room_label, is_available)
		SELECT * FROM unnest($1::text[], $2::text[], $3::int[], $4::text[], $5::bool[])
	`

	var (
		ids          = make([]string, len(rooms))
		hotelIDs     = make([]string, len(rooms))
		typeIDs      = make([]int, len(rooms))
		labels       = make([]string, len(rooms))
		availability = make([]bool, len(rooms))
	)

	for i, room := range rooms {
		ids[i] = room.ID
		hotelIDs[i] = room.HotelID
		typeIDs[i] = room.TypeID
		labels[i] = room.Label
		availability[i] = room.IsAvailable
	}

	if _, err := r.db.ExecContext(
		ctx,
		query,
		pq.Array(ids),
		pq.Array(hotelIDs),
		pq.Array(typeIDs),
		pq.Array(labels),
		pq.Array(availability),
	); err != nil {
		return fmt.Errorf("insert rooms: %w", err)
	}

	return nil
}

// DeleteAllRooms removes all rooms together with their dependent records.
func (r *roomRepository) DeleteAllRooms(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	if _, err := r.db.ExecContext(ctx, `DELETE FROM rooms`); err != nil {
		return fmt.Errorf("delete all rooms: %w", err)
	}

	return nil
}
