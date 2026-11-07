package upstream

import "testing"

// O login que vence no meio deixa a mesma tela de antes; a frase precisa dizer
// por que falhou, senão o admin repete o mesmo passo devagar.
func TestSituacao_LoginExpirado(t *testing.T) {
	var d Detalhe
	d.Habilitado = true
	d.Estado = EstadoSemConsentimento
	d.UltimoErro = "upstream x: upstream: sem consentimento OAuth: consentimento não concluído no prazo"
	if _, titulo, _ := situacao(d); titulo != "O login expirou antes de terminar" {
		t.Fatalf("título = %q", titulo)
	}
	d.UltimoErro = ""
	if _, titulo, _ := situacao(d); titulo != "Falta conectar a sua conta" {
		t.Fatalf("título = %q", titulo)
	}
}
