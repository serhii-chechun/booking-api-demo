package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"booking-api-demo/internal/reservation/model"

	"github.com/lib/pq"
	"github.com/lib/pq/pqerror"
)

type (
	reservationRepository struct {
		db           *sql.DB
		queryTimeout time.Duration
	}

	rowScanner interface {
		Scan(dest ...any) error
	}
)

// New creates a new instance of reservationRepository with the provided sql.DB.
func New(db *sql.DB, queryTimeout time.Duration) *reservationRepository {
	return &reservationRepository{
		db:           db,
		queryTimeout: queryTimeout,
	}
}

const reservationSelect = `
	SELECT res.id,
	       res.check_in_date::text, res.check_out_date::text,
	       res.guest_full_name, res.guest_email, res.guests_count,
	       res.reference, res.reservation_status,
	       res.hotel_id,
	       h.name, h.address
	FROM %s AS res
	INNER JOIN hotels AS h ON h.id = res.hotel_id
`

const reservationRoomsSelect = `
	SELECT rr.reservation_id, rr.room_id, rr.guests_count,
	       rm.room_label, rt.caption, rt.capacity
	FROM reservation_rooms AS rr
	INNER JOIN rooms AS rm ON rm.id = rr.room_id
	INNER JOIN room_types AS rt ON rt.id = rm.room_type_id
	WHERE rr.reservation_id = ANY($1)
	ORDER BY rr.reservation_id, rm.room_label
`

type candidateRoom struct {
	ID               string
	Label            string
	RoomTypeCaption  string
	RoomTypeCapacity int
}

func scanReservation(s rowScanner) (*model.ReservationItem, error) {
	var (
		result model.ReservationItem
		hotel  model.ReservationHotelDetails
	)
	if err := s.Scan(
		&result.ID,
		&result.CheckInDate,
		&result.CheckOutDate,
		&result.GuestFullName,
		&result.GuestEmail,
		&result.GuestsCount,
		&result.Reference,
		&result.Status,
		&result.HotelID,
		&hotel.Name,
		&hotel.Address,
	); err != nil {
		return nil, err
	}

	result.ReservationHotelDetails = &hotel

	return &result, nil
}

func (r *reservationRepository) loadRooms(ctx context.Context, reservations []*model.ReservationItem) error {
	if len(reservations) == 0 {
		return nil
	}

	ids := make([]string, len(reservations))
	byID := make(map[string]*model.ReservationItem, len(reservations))
	for i, reservation := range reservations {
		ids[i] = reservation.ID
		byID[reservation.ID] = reservation
	}

	rows, err := r.db.QueryContext(ctx, reservationRoomsSelect, pq.Array(ids))
	if err != nil {
		return fmt.Errorf("get reservation rooms: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			reservationID string
			room          model.ReservationRoomDetails
		)
		if err := rows.Scan(
			&reservationID,
			&room.RoomID,
			&room.GuestsCount,
			&room.Label,
			&room.RoomTypeCaption,
			&room.RoomTypeCapacity,
		); err != nil {
			return fmt.Errorf("scan reservation room: %w", err)
		}

		if reservation, ok := byID[reservationID]; ok {
			reservation.Rooms = append(reservation.Rooms, &room)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate reservation rooms: %w", err)
	}

	return nil
}

// PutReservation allocates the rooms that will hold the party for the requested
// dates and inserts the booking header together with its room allocations.
func (r *reservationRepository) PutReservation(ctx context.Context, res model.ReservationItem) (*model.ReservationItem, error) {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	var hotelExists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM hotels WHERE id = $1)`, res.HotelID).Scan(&hotelExists); err != nil {
		return nil, fmt.Errorf("check hotel exists: %w", err)
	}
	if !hotelExists {
		return nil, model.ErrHotelNotFound
	}

	allocations, err := allocateReservationRooms(ctx, tx, res)
	if err != nil {
		return nil, err
	}

	insert := `
		WITH inserted AS (
			INSERT INTO reservations (
				id, reference, hotel_id, guest_full_name, guest_email,
				guests_count, check_in_date, check_out_date, reservation_status
			)
			VALUES (gen_random_uuid()::text, $1, $2, $3, $4, $5, $6::date, $7::date, $8)
			RETURNING *
		)
	` + fmt.Sprintf(reservationSelect, "inserted")

	result, err := scanReservation(tx.QueryRowContext(ctx, insert,
		res.Reference,
		res.HotelID,
		res.GuestFullName,
		res.GuestEmail,
		res.GuestsCount,
		res.CheckInDate,
		res.CheckOutDate,
		int(res.Status),
	))
	if err != nil {
		var pqErr *pq.Error
		if errors.As(err, &pqErr) && pqErr.Code == pqerror.UniqueViolation {
			return nil, model.ErrReferenceExists
		}
		return nil, fmt.Errorf("insert reservation: %w", err)
	}

	if err := insertReservationRooms(ctx, tx, result.ID, allocations); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit transaction: %w", err)
	}

	result.Rooms = allocations

	return result, nil
}

func allocateReservationRooms(ctx context.Context, tx *sql.Tx, res model.ReservationItem) ([]*model.ReservationRoomDetails, error) {
	if len(res.RoomIDs) > 0 {
		return lockRequestedRooms(ctx, tx, res)
	}

	candidates, err := lockAvailableRooms(ctx, tx, res)
	if err != nil {
		return nil, err
	}

	return allocateRooms(candidates, res.GuestsCount)
}

func lockRequestedRooms(ctx context.Context, tx *sql.Tx, res model.ReservationItem) ([]*model.ReservationRoomDetails, error) {
	roomIDs := slices.Compact(slices.Sorted(slices.Values(res.RoomIDs)))

	var known int
	if err := tx.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM rooms WHERE hotel_id = $1 AND id = ANY($2)`,
		res.HotelID,
		pq.Array(roomIDs),
	).Scan(&known); err != nil {
		return nil, fmt.Errorf("check requested rooms: %w", err)
	}
	if known != len(roomIDs) {
		return nil, model.ErrRoomNotFound
	}

	const query = `
		SELECT r.id, r.room_label, rt.caption, rt.capacity
		FROM rooms AS r
		INNER JOIN room_types AS rt ON rt.id = r.room_type_id
		WHERE r.hotel_id = $1
		  AND r.id = ANY($2)
		  AND r.is_available
		  AND NOT EXISTS (
		      SELECT 1
		      FROM reservation_rooms AS rr
		      INNER JOIN reservations AS res ON res.id = rr.reservation_id
		      WHERE rr.room_id = r.id
		        AND res.reservation_status <> $3
		        AND res.check_in_date < $5::date
		        AND res.check_out_date > $4::date
		  )
		ORDER BY r.id
		FOR UPDATE OF r
	`

	rows, err := tx.QueryContext(ctx, query, res.HotelID, pq.Array(roomIDs), int(model.ReservationStatusCancelled), res.CheckInDate, res.CheckOutDate)
	if err != nil {
		return nil, fmt.Errorf("lock requested rooms: %w", err)
	}
	defer rows.Close()

	rooms := make([]candidateRoom, 0, len(roomIDs))
	for rows.Next() {
		var room candidateRoom
		if err := rows.Scan(&room.ID, &room.Label, &room.RoomTypeCaption, &room.RoomTypeCapacity); err != nil {
			return nil, fmt.Errorf("scan requested room: %w", err)
		}
		rooms = append(rooms, room)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate requested rooms: %w", err)
	}
	if len(rooms) != len(roomIDs) {
		return nil, model.ErrRoomUnavailable
	}

	slices.SortStableFunc(rooms, func(a, b candidateRoom) int {
		return b.RoomTypeCapacity - a.RoomTypeCapacity
	})

	return distributeGuests(rooms, res.GuestsCount)
}

func lockAvailableRooms(ctx context.Context, tx *sql.Tx, res model.ReservationItem) ([]candidateRoom, error) {
	const query = `
		SELECT r.id, r.room_label, rt.caption, rt.capacity
		FROM rooms AS r
		INNER JOIN room_types AS rt ON rt.id = r.room_type_id
		WHERE r.hotel_id = $1
		  AND r.is_available
		  AND NOT EXISTS (
		      SELECT 1
		      FROM reservation_rooms AS rr
		      INNER JOIN reservations AS res ON res.id = rr.reservation_id
		      WHERE rr.room_id = r.id
		        AND res.reservation_status <> $2
		        AND res.check_in_date < $4::date
		        AND res.check_out_date > $3::date
		  )
		ORDER BY r.id
		FOR UPDATE OF r
	`

	rows, err := tx.QueryContext(ctx, query, res.HotelID, int(model.ReservationStatusCancelled), res.CheckInDate, res.CheckOutDate)
	if err != nil {
		return nil, fmt.Errorf("lock available rooms: %w", err)
	}
	defer rows.Close()

	rooms := make([]candidateRoom, 0)
	for rows.Next() {
		var room candidateRoom
		if err := rows.Scan(&room.ID, &room.Label, &room.RoomTypeCaption, &room.RoomTypeCapacity); err != nil {
			return nil, fmt.Errorf("scan candidate room: %w", err)
		}
		rooms = append(rooms, room)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate candidate rooms: %w", err)
	}

	return rooms, nil
}

func allocateRooms(candidates []candidateRoom, guestsCount int) ([]*model.ReservationRoomDetails, error) {
	single := -1
	for i := range candidates {
		if candidates[i].RoomTypeCapacity < guestsCount {
			continue
		}
		if single == -1 || candidates[i].RoomTypeCapacity < candidates[single].RoomTypeCapacity {
			single = i
		}
	}
	if single != -1 {
		return []*model.ReservationRoomDetails{newRoomAllocation(candidates[single], guestsCount)}, nil
	}

	sorted := slices.Clone(candidates)
	slices.SortStableFunc(sorted, func(a, b candidateRoom) int {
		return b.RoomTypeCapacity - a.RoomTypeCapacity
	})

	return distributeGuests(sorted, guestsCount)
}

func distributeGuests(rooms []candidateRoom, guestsCount int) ([]*model.ReservationRoomDetails, error) {
	remaining := guestsCount
	allocations := make([]*model.ReservationRoomDetails, 0, len(rooms))
	for _, room := range rooms {
		if remaining <= 0 {
			break
		}

		assigned := min(room.RoomTypeCapacity, remaining)
		allocations = append(allocations, newRoomAllocation(room, assigned))
		remaining -= assigned
	}

	if remaining > 0 {
		return nil, model.ErrInsufficientCapacity
	}

	return allocations, nil
}

func newRoomAllocation(room candidateRoom, guestsCount int) *model.ReservationRoomDetails {
	return &model.ReservationRoomDetails{
		RoomID:           room.ID,
		Label:            room.Label,
		RoomTypeCaption:  room.RoomTypeCaption,
		RoomTypeCapacity: room.RoomTypeCapacity,
		GuestsCount:      guestsCount,
	}
}

func insertReservationRooms(ctx context.Context, tx *sql.Tx, reservationID string, allocations []*model.ReservationRoomDetails) error {
	if len(allocations) == 0 {
		return nil
	}

	const query = `
		INSERT INTO reservation_rooms (reservation_id, room_id, guests_count)
		SELECT * FROM unnest($1::text[], $2::text[], $3::int[])
	`

	var (
		reservationIDs = make([]string, len(allocations))
		roomIDs        = make([]string, len(allocations))
		guestsCounts   = make([]int, len(allocations))
	)
	for i, allocation := range allocations {
		reservationIDs[i] = reservationID
		roomIDs[i] = allocation.RoomID
		guestsCounts[i] = allocation.GuestsCount
	}

	if _, err := tx.ExecContext(
		ctx,
		query,
		pq.Array(reservationIDs),
		pq.Array(roomIDs),
		pq.Array(guestsCounts),
	); err != nil {
		return fmt.Errorf("insert reservation rooms: %w", err)
	}

	return nil
}

// GetReservationByReference retrieves a reservation by its booking reference from the database.
func (r *reservationRepository) GetReservationByReference(ctx context.Context, bookingRef string) (*model.ReservationItem, error) {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	query := fmt.Sprintf(reservationSelect, "reservations") + " WHERE res.reference = $1"

	reservation, err := scanReservation(r.db.QueryRowContext(ctx, query, bookingRef))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get reservation by reference: %w", err)
	}

	if err := r.loadRooms(ctx, []*model.ReservationItem{reservation}); err != nil {
		return nil, err
	}

	return reservation, nil
}

// GetReservationByID retrieves a reservation by its ID from the database.
func (r *reservationRepository) GetReservationByID(ctx context.Context, reservationID string) (*model.ReservationItem, error) {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	query := fmt.Sprintf(reservationSelect, "reservations") + " WHERE res.id = $1"

	reservation, err := scanReservation(r.db.QueryRowContext(ctx, query, reservationID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get reservation by id: %w", err)
	}

	if err := r.loadRooms(ctx, []*model.ReservationItem{reservation}); err != nil {
		return nil, err
	}

	return reservation, nil
}

// UpdateReservationStatus updates the status of a reservation and returns the updated record.
func (r *reservationRepository) UpdateReservationStatus(ctx context.Context, reservationID string, status model.ReservationStatus) (*model.ReservationItem, error) {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	query := `
		WITH updated AS (
			UPDATE reservations
			SET reservation_status = $2
			WHERE id = $1
			RETURNING *
		)
	` + fmt.Sprintf(reservationSelect, "updated")

	reservation, err := scanReservation(r.db.QueryRowContext(ctx, query, reservationID, int(status)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, model.ErrReservationNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("update reservation status: %w", err)
	}

	if err := r.loadRooms(ctx, []*model.ReservationItem{reservation}); err != nil {
		return nil, err
	}

	return reservation, nil
}

// GetAllReservationsByStatus retrieves all reservations with the specified status from the database.
func (r *reservationRepository) GetAllReservationsByStatus(ctx context.Context, status model.ReservationStatus) ([]*model.ReservationItem, error) {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	query := fmt.Sprintf(reservationSelect, "reservations") + `
		WHERE res.reservation_status = $1
		ORDER BY res.check_in_date, res.id
	`

	rows, err := r.db.QueryContext(ctx, query, int(status))
	if err != nil {
		return nil, fmt.Errorf("get reservations by status: %w", err)
	}
	defer rows.Close()

	reservations := make([]*model.ReservationItem, 0)
	for rows.Next() {
		reservation, err := scanReservation(rows)
		if err != nil {
			return nil, fmt.Errorf("scan reservation: %w", err)
		}
		reservations = append(reservations, reservation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate reservations: %w", err)
	}

	if err := r.loadRooms(ctx, reservations); err != nil {
		return nil, err
	}

	return reservations, nil
}

// DeleteAllReservations removes all reservations.
func (r *reservationRepository) DeleteAllReservations(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, r.queryTimeout)
	defer cancel()

	if _, err := r.db.ExecContext(ctx, `DELETE FROM reservations`); err != nil {
		return fmt.Errorf("delete all reservations: %w", err)
	}

	return nil
}
