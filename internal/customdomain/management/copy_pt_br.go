package management

import (
	"fmt"
	"math"
	"time"
)

// CopyPTBR returns the user-facing PT-BR string for a Reason. Strings
// match the SIN-62259 spec verbatim so QA can assert against them.
//
// retryAfter / reservedUntil are interpolated when the reason is
// ReasonRateLimited / ReasonSlugReserved respectively; ignored otherwise.
func CopyPTBR(reason Reason, retryAfter time.Duration, reservedUntil *time.Time) string {
	switch reason {
	case ReasonInvalidHost:
		return "Domínio inválido. Use um FQDN válido sem IP literal e até 253 caracteres."
	case ReasonPrivateIP:
		return "Domínio aponta para IP privado. Use um IP público."
	case ReasonTokenMismatch:
		return "Registro TXT não encontrado ou valor incorreto. Verifique propagação DNS."
	case ReasonDNSResolutionFailed:
		return "Não foi possível resolver o DNS do domínio. Tente novamente em alguns minutos."
	case ReasonRateLimited:
		mins := int(math.Ceil(retryAfter.Minutes()))
		if mins < 1 {
			mins = 1
		}
		return fmt.Sprintf("Limite de domínios cadastrados por hora atingido. Tente novamente em %d minutos.", mins)
	case ReasonSlugReserved:
		when := "—"
		if reservedUntil != nil {
			when = reservedUntil.UTC().Format("02/01/2006")
		}
		return fmt.Sprintf("Este slug está reservado até %s. Escolha outro.", when)
	case ReasonNotFound:
		return "Domínio não encontrado."
	case ReasonForbidden:
		return "Acesso negado a este domínio."
	case ReasonAlreadyVerified:
		return "Este domínio já está verificado."
	case ReasonInternal:
		return "Erro interno. Tente novamente em alguns minutos."
	case ReasonTokenExpired:
		return "O token de verificação expirou. Gere um novo token e atualize o registro TXT no seu DNS."
	case ReasonTokenRotated:
		return "O token foi atualizado durante a verificação. Tente novamente."
	default:
		return ""
	}
}

// StatusLabelPTBR is the badge text per UI spec.
func StatusLabelPTBR(s Status) string {
	switch s {
	case StatusPending:
		return "Pendente"
	case StatusVerified:
		return "Verificado"
	case StatusPaused:
		return "Pausado"
	case StatusError:
		return "Erro"
	case StatusFailed:
		return "Falhou"
	default:
		return "Desconhecido"
	}
}

// StatusBadgeColor maps Status to one of four CSS classes used by the
// table partial. Kept in this package so HTTP code reuses the mapping.
// StatusFailed renders red because it carries the strongest "the user
// needs to act" signal (the verifier worker gave up and the row is now
// terminal — re-enrolling is the recovery path).
func StatusBadgeColor(s Status) string {
	switch s {
	case StatusPending:
		return "yellow"
	case StatusVerified:
		return "green"
	case StatusPaused:
		return "gray"
	case StatusError, StatusFailed:
		return "red"
	default:
		return "gray"
	}
}
