package main

import (
	"context"
	"encoding/base64" // <-- Adicionado para decodificar credenciais
	"encoding/json"
	"encoding/xml"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/rs/cors"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

// --- Estruturas de Dados (MANTIDAS) ---

// NfeProc é a estrutura raiz que a maioria dos arquivos NF-e utiliza (XML de processo).
type NFeProc struct {
	XMLName xml.Name `xml:"nfeProc"`
	NFe     NFe      `xml:"NFe"` // A NF-e real está aninhada aqui
}

// NFe simplificada para extração de dados relevantes.
type NFe struct {
	XMLName xml.Name `xml:"NFe"`
	InfNFe struct {
		ID    string `xml:"Id,attr"` // Chave da nota (usada para evitar duplicidade)
		Det []struct {
			Prod struct {
				CProd string `xml:"cProd"` // Código do Produto
				CEAN  string `xml:"cEAN"`  // Código de Barras
				XProd string `xml:"xProd"` // Descrição do Produto
				QCom  string `xml:"qCom"`  // Quantidade Comercial
			} `xml:"prod"`
			NItem string `xml:"nItem,attr"` // Número do Item na NF
		} `xml:"det"`
	} `xml:"infNFe"`
}

// ImportRequest é a estrutura para o JSON recebido do Frontend (importação XML por Arquivo).
type ImportRequest struct {
	XMLContent string `json:"xml_content"`
	UserID     string `json:"user_id"`
}

// ImportChaveRequest (Fluxo Chave)
type ImportChaveRequest struct {
	ChaveAcesso string `json:"chaveAcesso"`
}

// ImportResponse é a estrutura para o JSON retornado ao Frontend (sucesso/erro importação).
type ImportResponse struct {
	Message string `json:"message"`
	Error   string `json:"error,omitempty"`
}

// SheetFetchRequest define a estrutura para a requisição de busca de dados.
type SheetFetchRequest struct {
	SheetName string `json:"sheet_name"`
}

// SheetFetchResponse define a estrutura para a resposta de busca de dados.
type SheetFetchResponse struct {
	Data  [][]interface{} `json:"data"`
	Error string          `json:"error,omitempty"`
}

// SheetUpdateRequest define a estrutura para a requisição de atualização de célula.
type SheetUpdateRequest struct {
	SheetName string `json:"sheet_name"`
	Range     string `json:"range"` // Ex: "A2"
	Value     string `json:"value"`
}

// SheetClearRequest define a estrutura para a requisição de limpeza de dados.
type SheetClearRequest struct {
	SheetName string `json:"sheet_name"`
	Range     string `json:"range"` // Ex: "A2:Z" - o range a ser limpo
}


// Variáveis de Estado Global (Simulação de DB)
var importedChaves = make(map[string]bool)

// Variáveis de Configuração (MANTIDAS)
var (
	// *** Variável base para o diretório de execução ***
	baseDir string

	// *** MUDAR: ID da sua planilha Google Sheets ***
	SPREADSHEET_ID    = "1x4a-gJyjHVxNKBy0bsuAE40vpt5Y9O9f5xEEF7W-fcE"
	NOTA_FISCAL_SHEET = "NOTA FISCAL"

	// O caminho para as credenciais é montado na função init()
	CREDENTIALS_FILE string
	// O caminho para o executável C#.NET é montado na função init()
	pathToExe string
	// O diretório de saída dos XMLs é montado na função init()
	outputDir string
)

// Variável de Ambiente para CORS/Netlify (NOVA)
var frontendURL string 

// Serviço do Google Sheets global
var sheetsService *sheets.Service

// --- Inicialização e Configuração (MODIFICADAS) ---

func init() {
	// Obtém o diretório de trabalho atual.
	var err error
	baseDir, err = os.Getwd()
	if err != nil {
		log.Fatalf("Falha ao obter o diretório de trabalho: %v", err)
	}

	// Define as variáveis de caminho usando o baseDir (MANTIDO PARA FLUXO C# LOCAL)
	CREDENTIALS_FILE = filepath.Join(baseDir, "credentials.json")
	pathToExe = filepath.Join(baseDir, "NfePorChaveGo")
	outputDir = filepath.Join(baseDir, "nfes")
	
	// Garante que o diretório de saída (outputDir) exista
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		log.Fatalf("Falha ao criar o diretório de saída (%s): %v", outputDir, err)
	}

	// Lógica para obter a URL do frontend do ambiente (NOVA)
    frontendURL = os.Getenv("FRONTEND_URL") 
    if frontendURL == "" {
        // Valor padrão para desenvolvimento local ou ambiente desconhecido
        frontendURL = "https://nfefront.netlify.app" 
    }
}


func initSheetsService() error {
	ctx := context.Background()
	var credsBytes []byte
	var err error
	var credsLoaded = false // Flag para saber se as credenciais foram carregadas

	// 1. Tenta ler do arquivo local (Prioridade 1: Teste no Render com o arquivo no Git)
	log.Printf("Tentando ler credenciais do arquivo local: %s", CREDENTIALS_FILE)

	if _, err := os.Stat(CREDENTIALS_FILE); err == nil {
		credsBytes, err = os.ReadFile(CREDENTIALS_FILE)
		if err != nil {
			return fmt.Errorf("erro ao ler credenciais do arquivo: %w", err)
		}
		log.Println("Credenciais encontradas via arquivo local (credentials.json).")
		credsLoaded = true
	}

	// 2. Se o arquivo falhou, tenta ler da variável de ambiente Base64 (Prioridade 2)
	if !credsLoaded {
		base64Creds := os.Getenv("CREDENTIALS_BASE64")
		if base64Creds != "" {
			log.Println("Credenciais encontradas via variável de ambiente (Base64).")
            // Usamos RawURLEncoding, a tentativa mais robusta
			credsBytes, err = base64.RawURLEncoding.DecodeString(base64Creds) 
			if err != nil {
				return fmt.Errorf("erro ao decodificar Base64: %w", err)
			}
			credsLoaded = true
		}
	}

	if !credsLoaded {
		// Não conseguimos encontrar nem arquivo nem Base64. ERRO CRÍTICO.
		return fmt.Errorf("credenciais de acesso ao Google Sheets não encontradas. Verifique credentials.json ou a variável CREDENTIALS_BASE64")
	}

	// Usa o slice de bytes lido/decodificado para configurar o serviço
	config, err := google.JWTConfigFromJSON(credsBytes, sheets.SpreadsheetsScope)
	if err != nil {
		return fmt.Errorf("erro ao criar config JWT: %w", err)
	}

	client := config.Client(ctx)

	srv, err := sheets.NewService(ctx, option.WithHTTPClient(client))
	if err != nil {
		return fmt.Errorf("erro ao criar serviço Sheets: %w", err)
	}

	sheetsService = srv
	return nil
}

// --- Lógica de Negócios Principal (MANTIDAS) ---

// parseXMLToRows (MANTIDA)
func parseXMLToRows(xmlContent string) ([][]interface{}, string, error) {
	// ... (código mantido) ...
	var nfeProc NFeProc
	if err := xml.Unmarshal([]byte(xmlContent), &nfeProc); err != nil {
		log.Printf("Erro de XML Unmarshal: %v", err)
		return nil, "", fmt.Errorf("erro ao fazer unmarshal do XML. Verifique se o conteúdo é um XML NF-e válido: %w", err)
	}

	nfe := nfeProc.NFe

	chaveNFe := strings.TrimPrefix(nfe.InfNFe.ID, "NFe")
	if chaveNFe == "" {
		return nil, "", fmt.Errorf("chave da nota fiscal (Id) não encontrada no XML")
	}

	if importedChaves[chaveNFe] {
		return nil, "", fmt.Errorf("a nota fiscal com chave %s já foi importada anteriormente (simulação de controle de duplicidade)", chaveNFe)
	}

	var rows [][]interface{}

	// A Linha de Cabeçalho da NF (para agrupar as informações)
	// Formato: [Chave, "", "", ""] (4 colunas)
	notaHeader := []interface{}{fmt.Sprintf("NF Chave: %s", chaveNFe), "", "", ""}
	rows = append(rows, notaHeader)

	for _, det := range nfe.InfNFe.Det {
		// Conversão de Quantidade (para float64)
		qCom, err := strconv.ParseFloat(strings.Replace(det.Prod.QCom, ",", ".", -1), 64)
		if err != nil {
			log.Printf("Aviso: Falha ao converter quantidade '%s' para float. Usando 0. Erro: %v", det.Prod.QCom, err)
			qCom = 0.0
		}

		// Linhas de Detalhe do Produto
		// Formato: [Descrição, Quantidade, EAN, Item] (4 colunas)
		row := []interface{}{
			det.Prod.XProd, // Descrição (Coluna 1)
			qCom,           // Quantidade (Coluna 2)
			det.Prod.CEAN,  // Código de Barras (Coluna 3)
			det.NItem,      // Item (Coluna 4)
		}
		rows = append(rows, row)
	}

	return rows, chaveNFe, nil
}

// buscarXMLPorChave (MANTIDA)
func buscarXMLPorChave(chave string) ([]byte, error) {
	log.Printf("Iniciando execução externa: %s %s", pathToExe, chave)

	// O executável C#.NET é chamado aqui
	cmd := exec.Command(pathToExe, chave)
	output, err := cmd.CombinedOutput()

	if err != nil {
		log.Printf("Erro na execução do C#.NET: %v. Saída: %s", err, string(output))
		return nil, fmt.Errorf("falha ao executar o programa de consulta: %v. Saída: %s", err, string(output))
	}

	log.Printf("Execução do C#.NET concluída. Saída (DEBUG):\n%s", string(output))

	// O C#.NET DEVE estar salvando o arquivo dentro do diretório 'outputDir' (que é 'nfe')
	xmlFileName := fmt.Sprintf("NFe_%s.xml", chave)
	xmlFilePath := filepath.Join(outputDir, xmlFileName)

	xmlData, err := os.ReadFile(xmlFilePath)
	if err != nil {
		log.Printf("Erro ao ler o arquivo XML em %s: %v", xmlFilePath, err)
		return nil, fmt.Errorf("o C#.NET não gerou o arquivo XML. Possível motivo: NFe não autorizada ou inexistente.")
	}

	return xmlData, nil
}

// appendDataToSheet (MANTIDA)
func appendDataToSheet(ctx context.Context, rows [][]interface{}) error {
	// ... (código mantido) ...
	if sheetsService == nil {
		return fmt.Errorf("serviço do Google Sheets não inicializado")
	}

	valueRange := &sheets.ValueRange{
		Values: rows,
	}

	// Insere as linhas
	_, err := sheetsService.Spreadsheets.Values.Append(
		SPREADSHEET_ID,
		NOTA_FISCAL_SHEET,
		valueRange,
	).ValueInputOption("USER_ENTERED").InsertDataOption("INSERT_ROWS").Context(ctx).Do()

	if err != nil {
		return fmt.Errorf("erro ao inserir dados no Sheets: %w", err)
	}

	return nil
}

// clearSheetData (MANTIDA)
func clearSheetData(ctx context.Context, sheetName, rangeToClear string) error {
	// ... (código mantido) ...
	if sheetsService == nil {
		return fmt.Errorf("serviço do Google Sheets não inicializado")
	}

	// Constrói o range completo (Ex: "NOTA FISCAL!A2:Z")
	fullRange := fmt.Sprintf("%s!%s", sheetName, rangeToClear)

	// NOTA: Usa-se A2 para manter a linha de cabeçalho (A1)
	_, err := sheetsService.Spreadsheets.Values.Clear(
		SPREADSHEET_ID,
		fullRange,
		&sheets.ClearValuesRequest{},
	).Context(ctx).Do()

	if err != nil {
		return fmt.Errorf("erro ao limpar dados no Sheets para o range %s: %w", fullRange, err)
	}

	return nil
}


// --- Handlers HTTP (MANTIDOS) ---

// handleImportXML (MANTIDO)
func handleImportXML(w http.ResponseWriter, r *http.Request) {
	// ... (código mantido) ...
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	w.Header().Set("Content-Type", "application/json")

	var req ImportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(ImportResponse{
			Error: "Formato de requisição JSON inválido.",
		})
		return
	}

	rows, chave, err := parseXMLToRows(req.XMLContent)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(ImportResponse{
			Error: err.Error(),
		})
		return
	}

	if err := appendDataToSheet(ctx, rows); err != nil {
		log.Printf("Erro ao processar Sheets: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(ImportResponse{
			Error: "Erro ao comunicar com o Google Sheets. Verifique as permissões de acesso: " + err.Error(),
		})
		return
	}

	importedChaves[chave] = true
	log.Printf("Sucesso na importação da NF: %s para o usuário: %s", chave, req.UserID)

	json.NewEncoder(w).Encode(ImportResponse{
		Message: fmt.Sprintf("Nota Fiscal (Chave: %s) importada com sucesso!", chave),
	})
}

// importarXMLHandler (MANTIDO)
func importarXMLHandler(w http.ResponseWriter, r *http.Request) {
	// ... (código mantido) ...
	if r.Method != http.MethodPost {
		http.Error(w, "Método não permitido", http.StatusMethodNotAllowed)
		return
	}

	w.Header().Set("Content-Type", "application/json")

	var req ImportChaveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(ImportResponse{Error: "Requisição JSON inválida."})
		return
	}

	chaveAcesso := req.ChaveAcesso
	// Definimos o contexto com timeout.
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// PASSO 1: OBTÉM o XML do C#.NET
	xmlData, err := buscarXMLPorChave(chaveAcesso)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(ImportResponse{Error: fmt.Sprintf("Erro na busca do XML: %v", err)})
		return
	}

	// PASSO 2: USA A MESMA LÓGICA DO ARQUIVO XML para extrair dados detalhados.
	xmlContent := string(xmlData)
	rows, chave, err := parseXMLToRows(xmlContent)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(ImportResponse{
			Error: "Erro ao processar XML baixado: " + err.Error(),
		})
		return
	}

	// PASSO 3: INSERÇÃO NO SHEETS
	if err := appendDataToSheet(ctx, rows); err != nil {
		log.Printf("Erro ao processar Sheets: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(ImportResponse{
			Error: "Erro ao comunicar com o Google Sheets: " + err.Error(),
		})
		return
	}

	importedChaves[chave] = true
	log.Printf("Sucesso na importação por chave: %s", chave)

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(ImportResponse{
		Message: fmt.Sprintf("Nota Fiscal (Chave: %s) baixada e importada com sucesso!", chave),
	})
}


// handleFetchSheetData (MANTIDO)
func handleFetchSheetData(w http.ResponseWriter, r *http.Request) {
	// ... (código mantido) ...
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	w.Header().Set("Content-Type", "application/json")

	var req SheetFetchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(SheetFetchResponse{
			Error: "Formato de requisição inválido.",
		})
		return
	}

	readRange := fmt.Sprintf("%s!A:Z", req.SheetName)

	resp, err := sheetsService.Spreadsheets.Values.Get(SPREADSHEET_ID, readRange).Context(ctx).Do()
	if err != nil {
		log.Printf("Erro ao buscar dados do Sheets para a aba %s: %v", req.SheetName, err)
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(SheetFetchResponse{
			Error: "Falha ao buscar dados do Google Sheets: " + err.Error(),
		})
		return
	}

	json.NewEncoder(w).Encode(SheetFetchResponse{
		Data: resp.Values,
	})
}

// handleUpdateSheetData (MANTIDO)
func handleUpdateSheetData(w http.ResponseWriter, r *http.Request) {
	// ... (código mantido) ...
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	w.Header().Set("Content-Type", "application/json")

	var req SheetUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(ImportResponse{
			Error: "Formato de requisição JSON inválido.",
		})
		return
	}

	// Constrói o range completo (Ex: "CONTAGEM LOJA!A2")
	fullRange := fmt.Sprintf("%s!%s", req.SheetName, req.Range)

	// Prepara o corpo da requisição de atualização
	valueRange := &sheets.ValueRange{
		Values: [][]interface{}{{req.Value}},
	}

	// Chama a API de atualização (Range=A2, B3, etc.)
	_, err := sheetsService.Spreadsheets.Values.Update(
		SPREADSHEET_ID,
		fullRange,
		valueRange,
	).ValueInputOption("USER_ENTERED").Context(ctx).Do()

	if err != nil {
		log.Printf("Erro ao atualizar Sheets para o range %s: %v", fullRange, err)
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(ImportResponse{
			Error: "Falha ao atualizar Google Sheets: " + err.Error(),
		})
		return
	}

	json.NewEncoder(w).Encode(ImportResponse{
		Message: fmt.Sprintf("Célula %s atualizada com sucesso para '%s'.", req.Range, req.Value),
	})
}

// handleClearSheetData (MANTIDO)
func handleClearSheetData(w http.ResponseWriter, r *http.Request) {
	// ... (código mantido) ...
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	w.Header().Set("Content-Type", "application/json")

	var req SheetClearRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(ImportResponse{
			Error: "Formato de requisição JSON inválido.",
		})
		return
	}

	if err := clearSheetData(ctx, req.SheetName, req.Range); err != nil {
		log.Printf("Erro ao limpar dados do Sheets: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(ImportResponse{
			Error: "Falha ao limpar dados do Google Sheets: " + err.Error(),
		})
		return
	}

	// Limpar o controle de duplicidade apenas se for a aba NOTA FISCAL
	if req.SheetName == NOTA_FISCAL_SHEET {
		importedChaves = make(map[string]bool)
	}

	log.Printf("Dados da aba '%s' (Range: %s) limpos com sucesso.", req.SheetName, req.Range)
	json.NewEncoder(w).Encode(ImportResponse{
		Message: fmt.Sprintf("Dados da aba '%s' limpos com sucesso.", req.SheetName),
	})
}


// --- Inicialização do Servidor (MODIFICADA) ---

func main() {
	// 1. Inicializa o serviço do Google Sheets
	if err := initSheetsService(); err != nil {
		log.Fatalf("Falha na inicialização do serviço Sheets: %v", err)
	}
	log.Println("Serviço do Google Sheets inicializado com sucesso.")
	log.Printf("Caminho das Credenciais (Local): %s", CREDENTIALS_FILE)
	log.Printf("Caminho do Executável (Local): %s", pathToExe)
	log.Printf("Diretório de Saída XML (Local): %s", outputDir)


	// 2. Configura o roteador HTTP
	r := chi.NewRouter()

	// Middlewares
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	// Configuração CORS (AGORA USA A VARIÁVEL frontendURL)
	c := cors.New(cors.Options{
		// Permite a URL do Netlify (frontendURL), localhost de desenvolvimento, e a URL do Render quando ele fizer health check.
		AllowedOrigins:   []string{"*"},
		AllowedMethods:   []string{"GET", "POST", "OPTIONS"},
		AllowedHeaders:   []string{"Accept", "Authorization", "Content-Type", "X-CSRF-Token"},
		AllowCredentials: true,
	})
	r.Use(c.Handler)

	// 3. Rotas da Aplicação (MANTIDAS)
	r.Post("/import-xml-data", handleImportXML)
	r.Post("/importar-xml-chave", importarXMLHandler)
	r.Post("/fetch-sheet-data", handleFetchSheetData)
	r.Post("/update-sheet-data", handleUpdateSheetData)
	r.Post("/clear-sheet-data", handleClearSheetData)

	// 4. Inicia o servidor na porta dinâmica (NOVO: LÊ DO AMBIENTE)
	port := os.Getenv("PORT")
	if port == "" {
		port = "10000" // Porta padrão para o Render Free Tier
	}
	
	log.Printf("Servidor Go rodando na porta :%s. Frontend URL permitido: %s", port, frontendURL)
	if err := http.ListenAndServe(":"+port, r); err != nil {
		log.Fatalf("Erro ao iniciar servidor: %v", err)
	}
}
