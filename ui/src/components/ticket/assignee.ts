// Option values for the Details panel Assignee field, shared by the ticket view and TicketDetails.
/** "Unassigned": choosing it on an assigned ticket starts the release flow (a reason is required). */
export const UNASSIGNED = '__unassigned__'
/** "Assign to me": claims an unassigned ticket (group members) or assigns via the assignments API (admins). */
export const ASSIGN_SELF = '__self__'
