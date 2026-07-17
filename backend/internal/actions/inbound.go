package actions

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

const maxEmailReplyInstruction = 4000

const maxCombinedEmailInstructions = 12000

// EmailReplyResult is the request created or revised by one inbound email.
type EmailReplyResult struct {
	Request   ActionRequestView
	Duplicate bool
}

// HandleEmailReply creates or revises a draft without authorizing a GitHub write.
func (s *Service) HandleEmailReply(messageID, targetKind, targetID, owner, generationToken, body string) (EmailReplyResult, error) {
	owner = strings.ToLower(strings.TrimSpace(owner))
	if owner == "" {
		return EmailReplyResult{}, fmt.Errorf("email reply owner is required")
	}
	receiptKey, err := emailMessageKey(messageID)
	if err != nil {
		return EmailReplyResult{}, err
	}
	if view, duplicate, err := s.lookupInboundReceipt(receiptKey, owner); duplicate || err != nil {
		return EmailReplyResult{Request: view, Duplicate: duplicate}, err
	}

	reply, err := cleanEmailReply(body)
	if err != nil {
		return EmailReplyResult{}, err
	}
	switch targetKind {
	case "pattern":
		kind, instruction, err := parsePatternReply(reply)
		if err != nil {
			return EmailReplyResult{}, err
		}
		view, duplicate, err := s.createRequest(targetID, kind, owner, generationToken, instruction, receiptKey)
		return EmailReplyResult{Request: view, Duplicate: duplicate}, err
	case "request":
		if forbiddenEmailCommand(reply) {
			return EmailReplyResult{}, fmt.Errorf("email replies cannot confirm or post a draft; use the authenticated dashboard")
		}
		view, duplicate, err := s.reviseRequestFromEmail(receiptKey, targetID, owner, generationToken, reply)
		return EmailReplyResult{Request: view, Duplicate: duplicate}, err
	default:
		return EmailReplyResult{}, fmt.Errorf("unsupported email reply target")
	}
}

func (s *Service) lookupInboundReceipt(receiptKey, owner string) (ActionRequestView, bool, error) {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	if s.expireRequestsLocked(time.Now().UTC()) {
		if err := s.saveRequestsLocked(); err != nil {
			return ActionRequestView{}, false, err
		}
	}
	return s.inboundRequestLocked(receiptKey, owner)
}

func (s *Service) reviseRequestFromEmail(receiptKey, id, owner, generationToken, instruction string) (ActionRequestView, bool, error) {
	now := time.Now().UTC()
	s.rmu.Lock()
	if s.expireRequestsLocked(now) {
		if err := s.saveRequestsLocked(); err != nil {
			s.rmu.Unlock()
			return ActionRequestView{}, false, err
		}
	}
	if existing, duplicate, err := s.inboundRequestLocked(receiptKey, owner); duplicate || err != nil {
		s.rmu.Unlock()
		return existing, duplicate, err
	}
	request := s.requests.Requests[id]
	if request == nil || request.Owner != owner {
		s.rmu.Unlock()
		return ActionRequestView{}, false, ErrRequestNotFound
	}
	if _, confirming := s.requestConfirms[id]; confirming {
		s.rmu.Unlock()
		return ActionRequestView{}, false, fmt.Errorf("action request is being confirmed")
	}
	if request.Status != RequestReady {
		status := request.Status
		s.rmu.Unlock()
		return ActionRequestView{}, false, fmt.Errorf("only a ready action request can be revised by email; request is %s", status)
	}
	pending := 0
	for requestID, existing := range s.requests.Requests {
		if requestID != id && existing.Status == RequestPending && existing.Owner == owner {
			pending++
		}
	}
	if pending >= maxPendingPerOwner {
		s.rmu.Unlock()
		return ActionRequestView{}, false, fmt.Errorf("too many pending action requests")
	}

	previous := *request
	combinedInstruction := appendEmailInstruction(request.Instruction, instruction)
	if len(combinedInstruction) > maxCombinedEmailInstructions {
		s.rmu.Unlock()
		return ActionRequestView{}, false, fmt.Errorf("combined email instructions exceed %d characters", maxCombinedEmailInstructions)
	}
	request.Status = RequestPending
	request.UpdatedAt = now.Format(time.RFC3339)
	request.ExpiresAt = now.Add(actionRequestTTL).Format(time.RFC3339)
	request.Error = ""
	request.ResultURL = ""
	request.Preview = nil
	request.EmailSent = false
	request.EmailError = ""
	request.Issue = nil
	request.Fix = nil
	request.Instruction = combinedInstruction
	request.Inbound = true
	s.requests.Inbound[receiptKey] = inboundReceipt{RequestID: id, ReceivedAt: now.Format(time.RFC3339)}
	if err := s.saveRequestsLocked(); err != nil {
		*request = previous
		delete(s.requests.Inbound, receiptKey)
		s.rmu.Unlock()
		return ActionRequestView{}, false, err
	}
	view := request.ActionRequestView
	s.rmu.Unlock()

	go s.generateRequest(id, generationToken)
	return view, false, nil
}

func emailMessageKey(messageID string) (string, error) {
	messageID = strings.TrimSpace(messageID)
	if messageID == "" || len(messageID) > 512 {
		return "", fmt.Errorf("inbound email message_id is required and must not exceed 512 characters")
	}
	sum := sha256.Sum256([]byte(messageID))
	return hex.EncodeToString(sum[:]), nil
}

func cleanEmailReply(body string) (string, error) {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	body = strings.ReplaceAll(body, "\r", "\n")
	var kept []string
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		if strings.HasPrefix(trimmed, ">") || strings.HasPrefix(lower, "on ") && strings.HasSuffix(lower, " wrote:") ||
			lower == "-----original message-----" || lower == "--" || strings.HasPrefix(lower, "sent from my ") {
			break
		}
		kept = append(kept, line)
	}
	cleaned := strings.TrimSpace(strings.Join(kept, "\n"))
	if cleaned == "" {
		return "", fmt.Errorf("email reply contains no new instructions")
	}
	if len(cleaned) > maxEmailReplyInstruction {
		return "", fmt.Errorf("email reply exceeds %d characters", maxEmailReplyInstruction)
	}
	return cleaned, nil
}

func parsePatternReply(reply string) (string, string, error) {
	first, rest, _ := strings.Cut(reply, "\n")
	first = strings.TrimSpace(first)
	command, inline, _ := strings.Cut(first, ":")
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(command)))
	if len(fields) != 1 || (fields[0] != "issue" && fields[0] != "fix") {
		return "", "", fmt.Errorf("reply with 'issue' or 'fix', followed by optional instructions")
	}
	instruction := strings.TrimSpace(strings.TrimSpace(inline) + "\n" + strings.TrimSpace(rest))
	if forbiddenEmailCommand(instruction) {
		return "", "", fmt.Errorf("email replies cannot confirm or post a draft; use the authenticated dashboard")
	}
	if fields[0] == "issue" {
		return "create-issue", instruction, nil
	}
	return "propose-fix", instruction, nil
}

func forbiddenEmailCommand(reply string) bool {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(reply)))
	if len(fields) == 0 {
		return false
	}
	switch strings.Trim(fields[0], "!.,:") {
	case "approve", "approved", "confirm", "confirmed", "merge", "open", "post", "publish", "send", "submit", "yes":
		return true
	default:
		return false
	}
}

func appendEmailInstruction(existing, instruction string) string {
	existing = strings.TrimSpace(existing)
	instruction = strings.TrimSpace(instruction)
	if existing == "" {
		return instruction
	}
	return existing + "\n\nAdditional email instructions:\n" + instruction
}
